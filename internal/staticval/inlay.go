// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// InlayHint is a value shown after a reference, or after a whole argument.
type InlayHint struct {
	// Pos is the end of the reference, or of the whole expression.
	Pos   hcl.Pos
	Label string
	// Ref is the reference as written, for the tooltip. For a hint on a
	// whole expression it is the argument's name, or empty.
	Ref string
	// Whole is true for a hint on the value of a whole expression (count,
	// for_each, a conditional, a template or a local) rather than after one
	// reference inside it.
	Whole bool
}

// DefaultInlayHintMaxLength is the default label length, in characters,
// excluding the leading prefix.
const DefaultInlayHintMaxLength = 40

// ValueHintPrefix starts the label of a hint after a reference, and
// WholeHintPrefix the label of a hint on a whole expression. The triangle
// is no HCL operator, so a hint cannot read as code: "= " made
// `count = var.on = false ? 1 : 0` look like a chained assignment, ": "
// reads as the conditional's colon, "≔" renders like "=" at the hint's
// size, "⇒" like a for expression's "=>", and arrows like the plan
// overlay's "before -> after". The hint's position tells the two apart.
const (
	ValueHintPrefix = "▸ "
	WholeHintPrefix = "▸ "
)

// InlayPolicy selects which value hints InlayHintsWith returns.
type InlayPolicy int

const (
	// InlayAll puts a hint after every var and local reference whose
	// value is known.
	InlayAll InlayPolicy = iota
	// InlayInformative leaves out what tells the reader little and shows
	// one hint for a whole expression where it has a known value:
	//   - count and for_each get the number of instances or their keys;
	//   - conditional expressions, templates with interpolations and
	//     local values get their value at the end;
	//   - no hints inside templates, where they split the string, or
	//     before an index or a splat;
	//   - no hints for null and empty values;
	//   - at most MaxPerLine hints on one line.
	// References inside a whole expression whose value is not known keep
	// their own hints.
	InlayInformative
)

// DefaultMaxHintsPerLine caps the hints of one line in informative mode.
const DefaultMaxHintsPerLine = 3

// InlayOptions configure InlayHintsWith.
type InlayOptions struct {
	Policy InlayPolicy
	// MaxLength caps the characters of a value in a label.
	MaxLength int
	// MaxPerLine caps the hints of one line with InlayInformative.
	MaxPerLine int
	// HideDefaults treats variables that only have their default as
	// unknown, so that nothing computed from a default gets a hint. It is
	// meant for a library module (see IsLibraryRoot), whose defaults are
	// placeholders its callers replace. It changes the evaluator for the
	// rest of the request.
	HideDefaults bool
}

// InlayHints returns a hint after every var and local reference in the
// file whose value is fully known and not sensitive. Only references that
// start inside rng are considered (a zero rng means the whole file).
//
// Some places get no hints at all: variable blocks, where they would only
// restate the variable's own value, and the places OpenTofu redacts, which
// are outputs marked sensitive and resource or data arguments that the
// provider schema marks sensitive.
func (ev *Evaluator) InlayHints(filename string, rng hcl.Range, maxLen int) []InlayHint {
	return ev.InlayHintsWith(filename, rng, InlayOptions{Policy: InlayAll, MaxLength: maxLen})
}

// InlayHintsWith is InlayHints with a policy (see InlayPolicy). The same
// places get no hints.
func (ev *Evaluator) InlayHintsWith(filename string, rng hcl.Range, opts InlayOptions) []InlayHint {
	f, ok := ev.mod.Files[filename]
	if !ok {
		return nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	if opts.MaxLength <= 0 {
		opts.MaxLength = DefaultInlayHintMaxLength
	}
	if opts.MaxPerLine <= 0 {
		opts.MaxPerLine = DefaultMaxHintsPerLine
	}
	if opts.HideDefaults {
		ev.hideDefaults()
	}

	c := &hintCollector{
		ev:   ev,
		rng:  rng,
		opts: opts,
		// A var or local reference has the same value wherever it is
		// written, so each distinct one is evaluated and rendered once:
		// a file may refer to one large value a thousand times.
		labels: make(map[string]string),
	}
	for _, block := range body.Blocks {
		switch block.Type {
		case "variable":
			continue
		case "output":
			if isTrue(block.Body.Attributes["sensitive"]) {
				continue
			}
		case "resource", "data":
			if len(block.Labels) == 2 {
				for name, attr := range block.Body.Attributes {
					if ev.isSensitiveArgument(block.Type, block.Labels[0], name) {
						continue
					}
					c.attribute(block, attr)
				}
				for _, nested := range block.Body.Blocks {
					c.block(nested)
				}
				continue
			}
		}
		c.block(block)
	}
	if ev.cancelled() {
		return nil
	}
	hints := c.hints
	sort.SliceStable(hints, func(i, j int) bool {
		return hints[i].Pos.Byte < hints[j].Pos.Byte
	})
	if opts.Policy == InlayInformative {
		hints = capPerLine(hints, opts.MaxPerLine)
	}
	return hints
}

// hintCollector gathers the hints of one file.
type hintCollector struct {
	ev     *Evaluator
	rng    hcl.Range
	opts   InlayOptions
	labels map[string]string
	hints  []InlayHint
}

func (c *hintCollector) informative() bool {
	return c.opts.Policy == InlayInformative
}

// overlaps reports whether r overlaps the requested range.
func (c *hintCollector) overlaps(r hcl.Range) bool {
	return c.rng.End.Byte == 0 || (r.End.Byte >= c.rng.Start.Byte && r.Start.Byte <= c.rng.End.Byte)
}

// inRange reports whether a hint at pos belongs to the requested range.
func (c *hintCollector) inRange(pos hcl.Pos) bool {
	return c.rng.End.Byte == 0 || (pos.Byte >= c.rng.Start.Byte && pos.Byte <= c.rng.End.Byte)
}

func (c *hintCollector) block(b *hclsyntax.Block) {
	for _, attr := range b.Body.Attributes {
		c.attribute(b, attr)
	}
	for _, nested := range b.Body.Blocks {
		c.block(nested)
	}
}

func (c *hintCollector) attribute(owner *hclsyntax.Block, attr *hclsyntax.Attribute) {
	if c.informative() && refersToValues(attr.Expr) {
		switch {
		case isInstanceArgument(owner, attr.Name):
			if c.instancesHint(owner, attr) {
				return
			}
		case owner.Type == "locals":
			if c.wholeHint(attr.Name, attr.Expr, false) {
				return
			}
		}
	}
	c.expr(attr.Expr)
}

// expr adds the hints of an argument's value. In informative mode it
// descends into object and tuple constructors, so that an item on its
// own line is treated like an argument.
func (c *hintCollector) expr(e hclsyntax.Expression) {
	if !c.informative() {
		c.refs(e)
		return
	}
	switch e := e.(type) {
	case *hclsyntax.ConditionalExpr:
		if refersToValues(e) && c.wholeHint("", e, true) {
			return
		}
	case *hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr:
		// A hint inside a template splits the string; the template's
		// value on its own says more. Heredocs span lines, and their
		// values do not fit on one.
		if refersToValues(e) && e.Range().Start.Line == e.Range().End.Line {
			c.wholeHint("", e, false)
		}
		return
	case *hclsyntax.ObjectConsExpr:
		for _, item := range e.Items {
			c.refs(item.KeyExpr)
			c.expr(item.ValueExpr)
		}
		return
	case *hclsyntax.TupleConsExpr:
		for _, item := range e.Exprs {
			c.expr(item)
		}
		return
	}
	c.refs(e)
}

// wholeHint adds one hint with the value of e after its end, when the
// value is known and not sensitive. It reports whether e is dealt with:
// true also when e lies outside the requested range. keepEmpty keeps a
// null or empty value, which for a conditional says which branch won.
func (c *hintCollector) wholeHint(name string, e hclsyntax.Expression, keepEmpty bool) bool {
	rng := e.Range()
	if !c.overlaps(rng) {
		return true
	}
	r := c.ev.Eval(e, nil)
	if !r.IsKnown() || r.IsSensitive() {
		return false
	}
	if !keepEmpty && isEmptyValue(r.Value) {
		return true
	}
	c.add(InlayHint{Pos: rng.End, Label: WholeHintPrefix + FormatCompact(r.Value, c.opts.MaxLength), Ref: name, Whole: true})
	return true
}

// instancesHint adds one hint for the count or for_each of a resource,
// data source or module call: the number of instances, or the instance
// keys. It reports false when the instances are not known.
func (c *hintCollector) instancesHint(owner *hclsyntax.Block, attr *hclsyntax.Attribute) bool {
	rng := attr.Expr.Range()
	if !c.overlaps(rng) {
		return true
	}
	meta, r, insts, ok := c.ev.Instances(owner)
	if !ok || meta != attr.Name || !r.IsKnown() || r.IsSensitive() {
		return false
	}
	var label string
	switch {
	case meta == "count" || len(insts) == 0:
		label = fmt.Sprintf("%d %s", len(insts), plural(len(insts), "instance", "instances"))
	default:
		keys := make([]string, 0, len(insts))
		for _, inst := range insts {
			k, _ := inst.Key.Unmark()
			if k.Type() != cty.String || k.IsNull() || !k.IsKnown() {
				return false
			}
			keys = append(keys, formatKey(k.AsString()))
		}
		label = joinKeys(keys, c.opts.MaxLength)
	}
	c.add(InlayHint{Pos: rng.End, Label: WholeHintPrefix + label, Ref: attr.Name, Whole: true})
	return true
}

// joinKeys lists keys separated by commas in at most max characters,
// saying how many are left out.
func joinKeys(keys []string, max int) string {
	var b strings.Builder
	for i, k := range keys {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		more := ""
		if i < len(keys)-1 {
			more = fmt.Sprintf(", … %d more", len(keys)-i-1)
		}
		if i > 0 && utf8.RuneCountInString(b.String()+sep+k+more) > max {
			fmt.Fprintf(&b, ", … %d more", len(keys)-i)
			return b.String()
		}
		b.WriteString(sep)
		b.WriteString(truncate(k, max))
	}
	return b.String()
}

// refs adds a hint after every var and local reference in node.
func (c *hintCollector) refs(node hclsyntax.Node) {
	if node == nil {
		return
	}
	hclsyntax.Walk(node, &refWalker{c: c})
}

// refWalker visits the references of an expression, knowing whether each
// is inside a template and what its parent node is.
type refWalker struct {
	c         *hintCollector
	stack     []hclsyntax.Node
	templates int
}

func (w *refWalker) Enter(n hclsyntax.Node) hcl.Diagnostics {
	if expr, ok := n.(*hclsyntax.ScopeTraversalExpr); ok {
		var parent hclsyntax.Node
		if len(w.stack) > 0 {
			parent = w.stack[len(w.stack)-1]
		}
		w.c.ref(expr, parent, w.templates > 0)
	}
	switch n.(type) {
	case *hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr:
		w.templates++
	}
	w.stack = append(w.stack, n)
	return nil
}

func (w *refWalker) Exit(n hclsyntax.Node) hcl.Diagnostics {
	w.stack = w.stack[:len(w.stack)-1]
	switch n.(type) {
	case *hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr:
		w.templates--
	}
	return nil
}

func (c *hintCollector) ref(expr *hclsyntax.ScopeTraversalExpr, parent hclsyntax.Node, inTemplate bool) {
	if c.ev.cancelled() {
		return
	}
	exprRng := expr.Range()
	if c.rng.End.Byte > 0 && (exprRng.Start.Byte < c.rng.Start.Byte || exprRng.Start.Byte > c.rng.End.Byte) {
		return
	}
	switch expr.Traversal.RootName() {
	case "var", "local":
	default:
		return
	}
	if len(expr.Traversal) < 2 {
		return
	}
	if c.informative() && (inTemplate || isIndexed(expr, parent)) {
		return
	}
	key := traversalKey(expr.Traversal)
	label, seen := c.labels[key]
	if !seen {
		if r := c.ev.Eval(expr, nil); r.IsKnown() && !r.IsSensitive() && !(c.informative() && isEmptyValue(r.Value)) {
			label = ValueHintPrefix + FormatCompact(r.Value, c.opts.MaxLength)
		}
		c.labels[key] = label
	}
	if label == "" {
		return
	}
	c.add(InlayHint{
		Pos:   exprRng.End,
		Label: label,
		Ref:   TraversalString(expr.Traversal),
	})
}

func (c *hintCollector) add(h InlayHint) {
	if c.inRange(h.Pos) {
		c.hints = append(c.hints, h)
	}
}

// isInstanceArgument reports whether name is the count or for_each of a
// resource, data source, ephemeral resource or module call.
func isInstanceArgument(owner *hclsyntax.Block, name string) bool {
	if name != "count" && name != "for_each" {
		return false
	}
	switch owner.Type {
	case "resource", "data", "ephemeral", "module":
		return true
	}
	return false
}

// isIndexed reports whether a reference is indexed or splatted by its
// parent, as in local.subnets[each.key], where a hint would sit between
// the value and the index.
func isIndexed(expr *hclsyntax.ScopeTraversalExpr, parent hclsyntax.Node) bool {
	switch p := parent.(type) {
	case *hclsyntax.IndexExpr:
		return p.Collection == expr
	case *hclsyntax.SplatExpr:
		return p.Source == expr
	case *hclsyntax.RelativeTraversalExpr:
		return p.Source == expr
	}
	return false
}

// refersToValues reports whether an expression refers to a variable, a
// local or a module output. The value of anything else is either what
// the source shows (a literal, path.module) or not known statically, so
// a hint on the whole expression would say nothing new.
func refersToValues(e hclsyntax.Expression) bool {
	for _, t := range e.Variables() {
		switch t.RootName() {
		case "var", "local", "module":
			return true
		}
	}
	return false
}

// isEmptyValue reports whether a value is null, an empty string or an
// empty collection or structure: a hint for it says little.
func isEmptyValue(v cty.Value) bool {
	v, _ = v.UnmarkDeep()
	if v.IsNull() {
		return true
	}
	if !v.IsKnown() {
		return false
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		return v.AsString() == ""
	case ty.IsCollectionType() || ty.IsObjectType() || ty.IsTupleType():
		return v.LengthInt() == 0
	}
	return false
}

// capPerLine keeps the first max hints of every line; hints are sorted.
func capPerLine(hints []InlayHint, max int) []InlayHint {
	out := hints[:0:0]
	line, n := -1, 0
	for _, h := range hints {
		if h.Pos.Line != line {
			line, n = h.Pos.Line, 0
		}
		n++
		if n <= max {
			out = append(out, h)
		}
	}
	return out
}

// traversalKey identifies a traversal exactly, index keys included.
func traversalKey(t hcl.Traversal) string {
	var b strings.Builder
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			b.WriteString(s.Name)
		case hcl.TraverseAttr:
			b.WriteString(".")
			b.WriteString(s.Name)
		case hcl.TraverseIndex:
			b.WriteString("[")
			b.WriteString(s.Key.GoString())
			b.WriteString("]")
		case hcl.TraverseSplat:
			b.WriteString("[*]")
		default:
			b.WriteString("?")
		}
	}
	return b.String()
}

// isSensitiveArgument reports whether the provider schema marks an
// argument of a resource or data source type as sensitive.
func (ev *Evaluator) isSensitiveArgument(blockType, typeName, attr string) bool {
	if ev.host == nil || ev.host.Attribute == nil {
		return false
	}
	info, ok := ev.host.Attribute(blockType, typeName, attr)
	return ok && info.Sensitive
}

func isTrue(attr *hclsyntax.Attribute) bool {
	if attr == nil {
		return false
	}
	v, diags := attr.Expr.Value(nil)
	return !diags.HasErrors() && v.IsKnown() && !v.IsNull() && v.Type() == cty.Bool && v.True()
}
