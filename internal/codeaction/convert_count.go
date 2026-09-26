// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// toggleKey is the instance key of a block converted from count = cond ?
// 1 : 0.
const toggleKey = "this"

// countBlockAt returns the resource, data or module block with a count
// argument whose header or count argument holds the cursor.
func countBlockAt(r *request) (*hclsyntax.Block, *hclsyntax.Attribute, bool) {
	b := r.rng.Start.Byte
	for _, block := range r.body.Blocks {
		switch block.Type {
		case "resource", "data", "module":
		default:
			continue
		}
		attr, ok := block.Body.Attributes["count"]
		if !ok {
			continue
		}
		if within(attr.SrcRange, b) || blockHeaderHolds(block, b) {
			return block, attr, true
		}
	}
	return nil, nil, false
}

// countForm tells which conversion fits a count expression: length(L)
// gives the list L, and cond ? 1 : 0 (or 0 : 1, inverted) the condition.
func countForm(expr hclsyntax.Expression) (list, cond hclsyntax.Expression, inverted bool) {
	for {
		p, ok := expr.(*hclsyntax.ParenthesesExpr)
		if !ok {
			break
		}
		expr = p.Expression
	}
	switch e := expr.(type) {
	case *hclsyntax.FunctionCallExpr:
		if e.Name == "length" && len(e.Args) == 1 && !e.ExpandFinal {
			return e.Args[0], nil, false
		}
	case *hclsyntax.ConditionalExpr:
		t, f := literalInt(e.TrueResult), literalInt(e.FalseResult)
		switch {
		case t == 1 && f == 0:
			return nil, e.Condition, false
		case t == 0 && f == 1:
			return nil, e.Condition, true
		}
	}
	return nil, nil, false
}

// literalInt is the value of a literal whole number, or -1.
func literalInt(expr hclsyntax.Expression) int {
	for {
		p, ok := expr.(*hclsyntax.ParenthesesExpr)
		if !ok {
			break
		}
		expr = p.Expression
	}
	if n, ok := literalIndex(expr); ok {
		return n
	}
	return -1
}

// conversion is a count to for_each conversion of one block.
type conversion struct {
	block *hclsyntax.Block
	count *hclsyntax.Attribute
	file  sourceFile
	// addr is the address of the block, as steps: aws_instance.web,
	// data.aws_ami.x or module.m.
	addr []string
	// movable is false for a data source, which has no state to move.
	movable bool
	toggle  bool
	// list is L of count = length(L); cond and inverted are the condition
	// of count = cond ? 1 : 0 (inverted: 0 : 1).
	list     hclsyntax.Expression
	cond     hclsyntax.Expression
	inverted bool
	// keys are the instance keys by index.
	keys    []string
	sources []string
	edits   map[string][]Edit
	srcs    map[string][]byte
	order   []string
}

func (c *conversion) address() string {
	return strings.Join(c.addr, ".")
}

func (c *conversion) edit(file string, src []byte, e Edit) {
	if _, ok := c.srcs[file]; !ok {
		c.srcs[file] = src
		c.order = append(c.order, file)
	}
	c.edits[file] = append(c.edits[file], e)
}

func offerCountToForEach(r *request) []Action {
	block, count, ok := countBlockAt(r)
	if !ok {
		return nil
	}
	list, cond, _ := countForm(count.Expr)
	switch {
	case list != nil:
		c, err := analyzeCount(r, block, count, false)
		title := "Convert count to for_each"
		if err == nil {
			title = c.title()
		}
		return []Action{r.offer(refCountToForEach, KindRefactorRewrite, title, err)}
	case cond != nil:
		c, err := analyzeCount(r, block, count, true)
		title := fmt.Sprintf("Convert count to for_each keyed %q", toggleKey)
		if err == nil {
			title = c.title()
		}
		return []Action{r.offer(refToggleToForEach, KindRefactorRewrite, title, err)}
	}
	return []Action{r.offer(refCountToForEach, KindRefactorRewrite, "Convert count to for_each",
		Refusal("count is neither length(<list>) nor <condition> ? 1 : 0"))}
}

func (c *conversion) title() string {
	if c.toggle {
		return fmt.Sprintf("Convert count to for_each keyed %q", toggleKey)
	}
	title := "Convert count to for_each"
	if c.movable {
		n := len(c.keys)
		title += fmt.Sprintf(" with %d moved block", n)
		if n != 1 {
			title += "s"
		}
	}
	var vars []string
	for _, s := range c.sources {
		if s != "default" && !strings.HasPrefix(s, "module.") {
			vars = append(vars, s)
		}
	}
	if len(vars) > 0 {
		title += " (keys from " + strings.Join(vars, ", ") + ")"
	}
	return title
}

func resolveCountToForEach(r *request, toggle bool) (Action, error) {
	block, count, ok := countBlockAt(r)
	if !ok {
		return Action{}, Refusal("there is no count argument here")
	}
	c, err := analyzeCount(r, block, count, toggle)
	if err != nil {
		return Action{}, err
	}
	var edits []Edit
	for _, file := range c.order {
		edits = append(edits, laidOut(file, c.srcs[file], c.edits[file])...)
	}
	if c.movable && len(c.keys) > 0 {
		var moved strings.Builder
		for i, key := range c.keys {
			if i > 0 {
				moved.WriteString("\n")
			}
			fmt.Fprintf(&moved, "moved {\n  from = %s[%d]\n  to   = %s[%s]\n}\n", c.address(), i, c.address(), valueText(cty.StringVal(key)))
		}
		edits = append(edits, appendBlock(c.file.path, c.file.src, moved.String()))
		edits = mergeInsertions(edits)
	}
	return Action{Title: c.title(), Kind: KindRefactorRewrite, Edits: edits}, nil
}

// analyzeCount checks that the block converts mechanically and collects
// the edits (the moved blocks aside). The error says what was refused.
func analyzeCount(r *request, block *hclsyntax.Block, count *hclsyntax.Attribute, toggle bool) (*conversion, error) {
	c := &conversion{block: block, count: count, toggle: toggle, edits: map[string][]Edit{}, srcs: map[string][]byte{}}
	switch {
	case block.Type == "resource" && len(block.Labels) == 2:
		c.addr, c.movable = []string{block.Labels[0], block.Labels[1]}, true
	case block.Type == "data" && len(block.Labels) == 2:
		c.addr = []string{"data", block.Labels[0], block.Labels[1]}
	case block.Type == "module" && len(block.Labels) == 1:
		c.addr, c.movable = []string{"module", block.Labels[0]}, true
	default:
		return nil, Refusal("the block has no address")
	}
	if _, ok := block.Body.Attributes["for_each"]; ok {
		return nil, Refusal("the block has for_each already")
	}
	c.list, c.cond, c.inverted = countForm(count.Expr)
	if toggle && c.cond == nil {
		return nil, Refusal("count is not <condition> ? 1 : 0")
	}
	if !toggle && c.list == nil {
		return nil, Refusal("count is not length(<list>)")
	}

	dir := r.doc.dir()
	if json := r.env.jsonConfig(dir); json != "" {
		return nil, refusef("%s cannot be edited, and may refer to %s", json, c.address())
	}
	files, ok := moduleSources(r.env, r.doc)
	if !ok {
		return nil, Refusal("a file of the module has syntax errors")
	}
	c.file = files[0]
	var declared []string
	for _, f := range files {
		for _, b := range f.body.Blocks {
			if b.Type == block.Type && strings.Join(b.Labels, ".") == strings.Join(block.Labels, ".") {
				declared = append(declared, filepath.ToSlash(relTo(dir, f.path)))
			}
		}
	}
	if len(declared) > 1 {
		return nil, refusef("%s is declared more than once (%s)", c.address(), strings.Join(declared, ", "))
	}

	if toggle {
		c.keys = []string{toggleKey}
	} else if err := c.staticKeys(r); err != nil {
		return nil, err
	}
	if err := c.rewriteCountIndex(r); err != nil {
		return nil, err
	}
	if err := c.rewriteReferences(dir, files); err != nil {
		return nil, err
	}
	tests, ok := r.env.parsedFiles(r.env.testFiles(dir))
	if !ok {
		return nil, Refusal("a test file of the module has syntax errors")
	}
	if uses := usesIn(tests, refersTo(c.addr[0], c.addr[1:]...)); len(uses) > 0 {
		return nil, refusef("a test file refers to %s (%s)", c.address(), listUses(dir, uses))
	}

	src := c.file.src
	var value string
	if toggle {
		cond := sourceOf(src, c.cond)
		value = fmt.Sprintf("%s ? toset([%q]) : toset([])", cond, toggleKey)
		if c.inverted {
			value = fmt.Sprintf("%s ? toset([]) : toset([%q])", cond, toggleKey)
		}
	} else {
		value = "toset(" + sourceOf(src, c.list) + ")"
	}
	c.edit(c.file.path, src, replace(c.file.path, src, count.SrcRange.Start.Byte, count.SrcRange.End.Byte, "for_each = "+value))
	return c, nil
}

// staticKeys takes the instance keys from the static value of the list:
// it must be known, and hold distinct strings.
func (c *conversion) staticKeys(r *request) error {
	name := compactText(c.file.src, c.list)
	if r.env.StaticValue == nil {
		return refusef("the value of %s is not known statically", name)
	}
	sv := r.env.StaticValue(r.doc.dir(), c.list)
	if !sv.Known {
		reason := sv.Reason
		if reason == "" {
			reason = "it is not known without a plan"
		}
		return refusef("the value of %s is not known statically (%s), and the moved blocks need its keys", name, reason)
	}
	v := sv.Value
	if v.IsMarked() || containsMarks(v) {
		return refusef("%s is sensitive, and for_each keys cannot be", name)
	}
	ty := v.Type()
	if v.IsNull() || !(ty.IsListType() || ty.IsTupleType()) {
		return refusef("%s is not a list", name)
	}
	seen := map[string]bool{}
	for it := v.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if elem.IsNull() || !elem.IsKnown() || elem.Type() != cty.String {
			return refusef("%s holds a value that is not a string, and for_each keys must be strings", name)
		}
		key := elem.AsString()
		if seen[key] {
			return refusef("%s holds %q twice, and toset() would drop an instance", name, key)
		}
		seen[key] = true
		c.keys = append(c.keys, key)
	}
	c.sources = sv.Sources
	return nil
}

func containsMarks(v cty.Value) bool {
	if v.IsMarked() {
		return true
	}
	if !v.IsKnown() || v.IsNull() || !(v.Type().IsCollectionType() || v.Type().IsTupleType() || v.Type().IsObjectType()) {
		return false
	}
	for it := v.ElementIterator(); it.Next(); {
		if _, e := it.Element(); containsMarks(e) {
			return true
		}
	}
	return false
}

// compactText is the source of an expression on one line, for messages.
func compactText(src []byte, expr hclsyntax.Expression) string {
	return quoted(sourceOf(src, expr), 40)
}

// rewriteCountIndex rewrites the uses of count.index in the block:
// L[count.index] and element(L, count.index) become each.value, and for
// a toggle count.index becomes 0. Anything else is refused.
func (c *conversion) rewriteCountIndex(r *request) error {
	src := c.file.src
	dir := r.doc.dir()
	f := sourceFile{path: c.file.path, src: src, body: &hclsyntax.Body{Blocks: hclsyntax.Blocks{c.block}}}
	done := map[hclsyntax.Node]bool{}
	for _, u := range usesIn([]sourceFile{f}, refersTo("count")) {
		if u.attr == c.count {
			continue
		}
		if len(u.expr.Traversal) != 2 || !refersTo("count", "index")(u.expr.Traversal) {
			return refusef("%s at %s is not count.index", sourceOf(src, u.expr), u.where(dir))
		}
		if c.toggle {
			c.edit(f.path, src, replace(f.path, src, u.expr.SrcRange.Start.Byte, u.expr.SrcRange.End.Byte, "0"))
			continue
		}
		list := compact(src, c.list)
		switch p := u.parent.(type) {
		case *hclsyntax.IndexExpr:
			if p.Key == u.expr && compact(src, p.Collection) == list {
				if !done[p] {
					done[p] = true
					c.edit(f.path, src, replace(f.path, src, p.SrcRange.Start.Byte, p.SrcRange.End.Byte, "each.value"))
				}
				continue
			}
		case *hclsyntax.FunctionCallExpr:
			if p.Name == "element" && len(p.Args) == 2 && !p.ExpandFinal && p.Args[1] == u.expr && compact(src, p.Args[0]) == list {
				if !done[p] {
					done[p] = true
					c.edit(f.path, src, replace(f.path, src, p.Range().Start.Byte, p.Range().End.Byte, "each.value"))
				}
				continue
			}
		}
		return refusef("count.index at %s is used other than as %s[count.index], and each.value cannot replace it", u.where(dir), quoted(sourceOf(src, c.list), 40))
	}
	return nil
}

// rewriteReferences rewrites the references to the block in the module:
// addr[i] becomes addr["key"], and addr[*] values(addr)[*]. length(addr)
// and depends_on stay as they are. Anything else is refused.
func (c *conversion) rewriteReferences(dir string, files []sourceFile) error {
	n := len(c.addr)
	uses := usesIn(files, refersTo(c.addr[0], c.addr[1:]...))
	// the order of values(addr) is the order of its keys
	sorted := sort.StringsAreSorted(c.keys)
	for _, u := range uses {
		if u.file == c.file.path && u.chain[0].Range().Start.Byte == c.block.Range().Start.Byte {
			return refusef("%s refers to itself at %s", c.address(), u.where(dir))
		}
		switch u.chain[0].Type {
		case "moved", "import", "removed":
			return refusef("a %s block refers to %s at %s", u.chain[0].Type, c.address(), u.where(dir))
		}
		tr := u.expr.Traversal
		if len(tr) > n {
			idx, ok := tr[n].(hcl.TraverseIndex)
			if !ok {
				return refusef("%s at %s is not an instance of %s", traversalText(u.src, tr), u.where(dir), c.address())
			}
			i := -1
			if idx.Key.Type() == cty.Number && idx.Key.IsKnown() && !idx.Key.IsNull() {
				if bf := idx.Key.AsBigFloat(); bf.IsInt() {
					if v, acc := bf.Int64(); acc == 0 && v >= 0 && v < int64(len(c.keys)) {
						i = int(v)
					}
				}
			}
			if i < 0 {
				return refusef("%s at %s is not one of the %d instances", traversalText(u.src, tr[:n+1]), u.where(dir), len(c.keys))
			}
			c.edit(u.file, u.src, replace(u.file, u.src, idx.SrcRange.Start.Byte, idx.SrcRange.End.Byte, "["+valueText(cty.StringVal(c.keys[i]))+"]"))
			continue
		}
		switch p := u.parent.(type) {
		case *hclsyntax.SplatExpr:
			if p.Source == u.expr {
				if !sorted {
					return refusef("%s[*] at %s lists the instances in the order of %s, but values(%s) would list them by key", c.address(), u.where(dir), quoted(sourceOf(c.file.src, c.list), 40), c.address())
				}
				c.edit(u.file, u.src, replace(u.file, u.src, u.expr.SrcRange.Start.Byte, u.expr.SrcRange.End.Byte, "values("+c.address()+")"))
				continue
			}
		case *hclsyntax.FunctionCallExpr:
			if p.Name == "length" && len(p.Args) == 1 && p.Args[0] == u.expr {
				// the number of instances, of a list or a map
				continue
			}
		}
		if isAddressArgument(u.chain, u.attr) {
			continue
		}
		return refusef("%s at %s is a list of instances, which for_each turns into a map", c.address(), u.where(dir))
	}
	return nil
}

// isAddressArgument tells whether an argument takes references as
// addresses, whatever the instances: depends_on and the lifecycle
// arguments.
func isAddressArgument(chain []*hclsyntax.Block, attr *hclsyntax.Attribute) bool {
	if attr.Name == "depends_on" && len(chain) == 1 {
		return true
	}
	inner := chain[len(chain)-1]
	return inner.Type == "lifecycle" && (attr.Name == "replace_triggered_by" || attr.Name == "ignore_changes")
}
