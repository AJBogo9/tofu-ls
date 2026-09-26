// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// Kinds of the refactorings, as LSP code action kinds.
const (
	KindRefactor        = "refactor"
	KindRefactorExtract = "refactor.extract"
	KindRefactorInline  = "refactor.inline"
)

// The refactorings, as Refactoring.ID names them.
const (
	refExtractLocal      = "extract-local"
	refIntroduceVariable = "introduce-variable"
	refIntroduceTfvars   = "introduce-variable-tfvars"
	refInlineLocal       = "inline-local"
	refInlineLocalHere   = "inline-local-here"
	refCountToForEach    = "count-to-for-each"
	refToggleToForEach   = "count-toggle-to-for-each"
	refSafeDelete        = "safe-delete"
)

// Refactoring names a refactoring offered for a range of a document. Its
// edits are computed when the client resolves the action (see Resolve):
// clients ask for code actions on every cursor move, and the edits of
// most refactorings read the whole module.
type Refactoring struct {
	ID string
	// Range is the range the refactoring was offered for, with correct
	// byte offsets in the text it was offered on.
	Range hcl.Range
}

// Refusal says why a refactoring cannot be applied.
type Refusal string

func (r Refusal) Error() string {
	return string(r)
}

func refusef(format string, args ...interface{}) error {
	return Refusal(fmt.Sprintf(format, args...))
}

// ModuleCaller is a module block of another module which calls a module.
type ModuleCaller struct {
	// Dir is the directory of the calling module.
	Dir string
	// Name is the name of the module block.
	Name string
}

// StaticValue is what the static evaluator knows about an expression.
type StaticValue struct {
	Value cty.Value
	// Known is true when the whole value is known without a plan.
	Known bool
	// Reason says why the value is not known.
	Reason string
	// Sources name where the values of the variables it reads come from:
	// a tfvars file, "default" or a module call.
	Sources []string
}

// request is a request for the refactorings of a range of a document.
type request struct {
	env  Env
	doc  Document
	body *hclsyntax.Body
	rng  hcl.Range
	// invoked is true when the user asked for code actions. Otherwise the
	// client asks on its own, on every cursor move, and only a selected
	// expression gets offers, which read no other file.
	invoked bool
	// resolving is true when the edits are computed: then the module's
	// callers are all looked for, which an offer does not wait for.
	resolving bool
}

// callers returns the modules which call the document's module: all of
// them when resolving, else those indexed so far.
func (r *request) callers() ([]ModuleCaller, bool) {
	dir := r.doc.dir()
	if r.resolving || r.env.IndexedCallers == nil {
		if r.env.ModuleCallers == nil {
			return nil, true
		}
		return r.env.ModuleCallers(dir)
	}
	return r.env.IndexedCallers(dir), true
}

// Refactorings returns the refactorings offered for rng, a range with
// correct byte offsets in the document. Their Resolve is set and their
// edits are empty; a refactoring refused for a reason known without its
// edits is disabled with that reason.
func Refactorings(env Env, doc Document, rng hcl.Range, invoked bool) []Action {
	if doc.Vars || (!invoked && rng.Start.Byte == rng.End.Byte) {
		// a cursor move: nothing to offer, nothing to parse
		return nil
	}
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return nil
	}
	rng = trimRange(doc.Text, rng)
	r := &request{env: env, doc: doc, body: body, rng: rng, invoked: invoked}
	var actions []Action
	actions = append(actions, offerExtractLocal(r)...)
	actions = append(actions, offerIntroduceVariable(r)...)
	if invoked {
		// these work on what the cursor (or the selection's start) is on
		actions = append(actions, offerInlineLocal(r)...)
		actions = append(actions, offerCountToForEach(r)...)
		actions = append(actions, offerSafeDelete(r)...)
	}
	return actions
}

// Resolve computes the edits of a refactoring that Refactorings offered
// on the same text of the document. It fails with a Refusal when the
// refactoring cannot be applied.
func Resolve(env Env, doc Document, ref Refactoring) (Action, error) {
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return Action{}, Refusal("the file has syntax errors")
	}
	if ref.Range.Start.Byte < 0 || ref.Range.End.Byte > len(doc.Text) || ref.Range.Start.Byte > ref.Range.End.Byte {
		return Action{}, Refusal("the range is outside the document")
	}
	r := &request{env: env, doc: doc, body: body, rng: ref.Range, invoked: true, resolving: true}
	switch ref.ID {
	case refExtractLocal:
		return resolveExtractLocal(r)
	case refIntroduceVariable:
		return resolveIntroduceVariable(r, false)
	case refIntroduceTfvars:
		return resolveIntroduceVariable(r, true)
	case refInlineLocal:
		return resolveInlineLocal(r, false)
	case refInlineLocalHere:
		return resolveInlineLocal(r, true)
	case refCountToForEach, refToggleToForEach:
		return resolveCountToForEach(r, ref.ID == refToggleToForEach)
	case refSafeDelete:
		return resolveSafeDelete(r)
	}
	return Action{}, refusef("unknown refactoring %q", ref.ID)
}

// offer is an action which Resolve computes later.
func (r *request) offer(id, kind, title string, refused error) Action {
	a := Action{Title: title, Kind: kind, Resolve: &Refactoring{ID: id, Range: r.rng}}
	if refused != nil {
		a.Disabled = refused.Error()
	}
	return a
}

// trimRange shrinks a selection to the text it selects without the
// whitespace around it.
func trimRange(src []byte, rng hcl.Range) hcl.Range {
	s, e := rng.Start.Byte, rng.End.Byte
	if s < 0 || e > len(src) || s >= e {
		return rng
	}
	for s < e && isSpace(src[s]) {
		s++
	}
	for e > s && isSpace(src[e-1]) {
		e--
	}
	if s == e {
		return rangeAt(rng.Filename, src, rng.Start.Byte, rng.Start.Byte)
	}
	return rangeAt(rng.Filename, src, s, e)
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func holds(outer hcl.Range, s, e int) bool {
	return outer.Start.Byte <= s && e <= outer.End.Byte
}

// attributeAt returns the attribute holding the bytes s to e with the
// blocks from the top of the file down to the one holding it.
func attributeAt(body *hclsyntax.Body, s, e int) ([]*hclsyntax.Block, *hclsyntax.Attribute, bool) {
	var chain []*hclsyntax.Block
	for {
		for _, attr := range body.Attributes {
			if holds(attr.SrcRange, s, e) {
				return chain, attr, len(chain) > 0
			}
		}
		var next *hclsyntax.Block
		for _, block := range body.Blocks {
			if holds(block.Body.SrcRange, s, e) {
				next = block
				break
			}
		}
		if next == nil {
			return nil, nil, false
		}
		chain = append(chain, next)
		body = next.Body
	}
}

// exprSite is an expression of a document with what surrounds it.
type exprSite struct {
	expr hclsyntax.Expression
	// ancestors are the nodes from the attribute's value down to the
	// parent of expr; empty when expr is the whole value.
	ancestors []hclsyntax.Node
	// chain is the blocks from the top of the file down to the block
	// holding attr.
	chain []*hclsyntax.Block
	attr  *hclsyntax.Attribute
	// key is the key of the innermost object item whose value holds
	// expr, or "".
	key string
}

func (s *exprSite) top() *hclsyntax.Block {
	return s.chain[0]
}

func (s *exprSite) parent() hclsyntax.Node {
	if len(s.ancestors) == 0 {
		return nil
	}
	return s.ancestors[len(s.ancestors)-1]
}

// whole tells whether expr is the whole value of its attribute.
func (s *exprSite) whole() bool {
	return len(s.ancestors) == 0
}

// stackWalker keeps the path from the walk's root to the current node.
type stackWalker struct {
	stack []hclsyntax.Node
	enter func(node hclsyntax.Node, ancestors []hclsyntax.Node) bool
	done  bool
}

func (w *stackWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	if !w.done && w.enter(node, w.stack) {
		w.done = true
	}
	w.stack = append(w.stack, node)
	return nil
}

func (w *stackWalker) Exit(node hclsyntax.Node) hcl.Diagnostics {
	w.stack = w.stack[:len(w.stack)-1]
	return nil
}

// selectedExpr returns the outermost expression whose range is exactly
// the bytes s to e.
func selectedExpr(body *hclsyntax.Body, s, e int) (*exprSite, bool) {
	chain, attr, ok := attributeAt(body, s, e)
	if !ok {
		return nil, false
	}
	var site *exprSite
	w := &stackWalker{enter: func(node hclsyntax.Node, ancestors []hclsyntax.Node) bool {
		expr, ok := node.(hclsyntax.Expression)
		if !ok || expr.Range().Start.Byte != s || expr.Range().End.Byte != e {
			return false
		}
		site = &exprSite{expr: expr, ancestors: append([]hclsyntax.Node(nil), ancestors...), chain: chain, attr: attr}
		return true
	}}
	hclsyntax.Walk(attr.Expr, w)
	if site == nil {
		return nil, false
	}
	site.key = itemKey(site.ancestors, site.expr)
	return site, true
}

// pathAt returns the nodes of the attribute's value which hold the byte
// b, outermost first.
func pathAt(attr *hclsyntax.Attribute, b int) []hclsyntax.Node {
	var path []hclsyntax.Node
	hclsyntax.VisitAll(attr.Expr, func(node hclsyntax.Node) hcl.Diagnostics {
		if _, ok := node.(hclsyntax.ChildScope); ok {
			return nil
		}
		if rng := node.Range(); rng.Start.Byte <= b && b <= rng.End.Byte {
			if n := len(path); n == 0 || holds(path[n-1].Range(), rng.Start.Byte, rng.End.Byte) {
				path = append(path, node)
			}
		}
		return nil
	})
	return path
}

// siteOf returns the site of expr, a node of the attribute's value.
func siteOf(chain []*hclsyntax.Block, attr *hclsyntax.Attribute, expr hclsyntax.Expression) *exprSite {
	var site *exprSite
	w := &stackWalker{enter: func(node hclsyntax.Node, ancestors []hclsyntax.Node) bool {
		if node != expr {
			return false
		}
		site = &exprSite{expr: expr, ancestors: append([]hclsyntax.Node(nil), ancestors...), chain: chain, attr: attr}
		return true
	}}
	hclsyntax.Walk(attr.Expr, w)
	if site != nil {
		site.key = itemKey(site.ancestors, expr)
	}
	return site
}

// itemKey is the key of the innermost object item whose value holds
// expr, when it is a name or a literal string.
func itemKey(ancestors []hclsyntax.Node, expr hclsyntax.Node) string {
	child := expr
	for i := len(ancestors) - 1; i >= 0; i-- {
		if obj, ok := ancestors[i].(*hclsyntax.ObjectConsExpr); ok {
			for _, item := range obj.Items {
				if item.ValueExpr != child {
					continue
				}
				if name := hcl.ExprAsKeyword(item.KeyExpr); name != "" {
					return name
				}
				if k, ok := item.KeyExpr.(*hclsyntax.ObjectConsKeyExpr); ok {
					if name := hcl.ExprAsKeyword(k.Wrapped); name != "" {
						return name
					}
					if v, diags := k.Wrapped.Value(nil); !diags.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
						return v.AsString()
					}
				}
			}
		}
		child = ancestors[i]
	}
	return ""
}

// cursorSlot returns the value of the innermost argument or object item
// holding the byte b, which a refactoring at the cursor works on.
func cursorSlot(body *hclsyntax.Body, b int) (*exprSite, bool) {
	chain, attr, ok := attributeAt(body, b, b)
	if !ok {
		return nil, false
	}
	var slot hclsyntax.Expression = attr.Expr
	for _, node := range pathAt(attr, b) {
		obj, ok := node.(*hclsyntax.ObjectConsExpr)
		if !ok {
			continue
		}
		for _, item := range obj.Items {
			if within(item.ValueExpr.Range(), b) {
				slot = item.ValueExpr
			}
		}
	}
	site := siteOf(chain, attr, slot)
	return site, site != nil
}

// scopeNames returns the names which the site's surroundings bind and a
// value in a locals block could not see: the variables of enclosing for
// expressions and the iterators of enclosing dynamic blocks.
func (s *exprSite) scopeNames() map[string]bool {
	names := map[string]bool{}
	for _, node := range s.ancestors {
		if cs, ok := node.(hclsyntax.ChildScope); ok {
			for name := range cs.LocalNames {
				names[name] = true
			}
		}
	}
	for _, block := range s.chain {
		if block.Type != "dynamic" || len(block.Labels) != 1 {
			continue
		}
		name := block.Labels[0]
		if it, ok := block.Body.Attributes["iterator"]; ok {
			if kw := hcl.ExprAsKeyword(it.Expr); kw != "" {
				name = kw
			}
		}
		names[name] = true
	}
	return names
}

// unseen returns the first reference of expr to something a local value
// cannot see at the top of the module: each, count, self, or a name that
// the site's surroundings bind.
func (s *exprSite) unseen(src []byte) (string, bool) {
	scope := s.scopeNames()
	for _, tr := range s.expr.Variables() {
		root := tr.RootName()
		switch {
		case root == "each" || root == "count" || root == "self":
			return traversalText(src, tr), true
		case scope[root]:
			return traversalText(src, tr), true
		}
	}
	return "", false
}

func traversalText(src []byte, tr hcl.Traversal) string {
	rng := tr.SourceRange()
	if rng.End.Byte <= len(src) && rng.Start.Byte < rng.End.Byte {
		return string(src[rng.Start.Byte:rng.End.Byte])
	}
	return tr.RootName()
}

// metaArguments are the arguments of each block type that take
// addresses, names or literal values rather than expressions, where a
// reference to a local value or a variable is not allowed.
var metaArguments = map[string]map[string]bool{
	"resource":    {"depends_on": true, "provider": true},
	"data":        {"depends_on": true, "provider": true},
	"ephemeral":   {"depends_on": true, "provider": true},
	"module":      {"depends_on": true, "providers": true, "source": true, "version": true},
	"output":      {"depends_on": true, "description": true, "sensitive": true, "ephemeral": true},
	"provider":    {"alias": true, "version": true},
	"provisioner": {"when": true, "on_failure": true},
	"check":       {},
	"locals":      {},
}

// contextRefusal says why the site is not a place where a reference can
// replace its expression, or returns "" when it is. Top-level blocks not
// in allowed are refused.
func (s *exprSite) contextRefusal(allowed map[string]bool) string {
	top := s.top()
	if !allowed[top.Type] {
		return fmt.Sprintf("a %s block cannot refer to it", top.Type)
	}
	inner := s.chain[len(s.chain)-1]
	switch {
	case len(s.chain) == 1 && metaArguments[top.Type][s.attr.Name]:
		return fmt.Sprintf("the %s argument takes no references", s.attr.Name)
	case inner.Type == "provisioner" && metaArguments[inner.Type][s.attr.Name]:
		return fmt.Sprintf("the %s argument takes no references", s.attr.Name)
	case inner.Type == "lifecycle":
		return "lifecycle arguments take only literal values"
	case inner.Type == "dynamic" && s.attr.Name == "iterator":
		return "the iterator argument is a name"
	}
	// preconditions and postconditions within lifecycle may refer to
	// anything
	return ""
}

// nameCleaner replaces what cannot be part of a name.
var nameCleaner = regexp.MustCompile(`[^a-z0-9_]+`)

// snakeName turns text into a valid lower-case name.
func snakeName(text string) string {
	name := nameCleaner.ReplaceAllString(strings.ToLower(text), "_")
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	name = strings.Trim(name, "_")
	if name == "" {
		return ""
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "v_" + name
	}
	return name
}

// suggestedName derives a name for a new local value or variable from
// where the expression stands: the name of its block and its argument
// (or object key), such as bucket_name for the name argument of
// aws_s3_bucket.bucket.
func (s *exprSite) suggestedName() string {
	top := s.top()
	var prefix, base string
	switch top.Type {
	case "resource", "data", "ephemeral":
		if len(top.Labels) == 2 {
			prefix = top.Labels[1]
		}
	case "locals":
		prefix = s.attr.Name
	default:
		if len(top.Labels) > 0 {
			prefix = top.Labels[0]
		}
	}
	base = s.key
	switch {
	case base != "":
	case top.Type == "locals":
		base = "part"
	case top.Type == "output" && len(s.chain) == 1 && s.attr.Name == "value":
	default:
		base = s.attr.Name
	}
	prefix, base = snakeName(prefix), snakeName(base)
	switch {
	case prefix == "" && base == "":
		return "value"
	case prefix == "" || prefix == base:
		return base
	case base == "":
		return prefix
	}
	return prefix + "_" + base
}

// unique returns name, or name with the first free suffix _2, _3 and so
// on when taken has it.
func unique(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", name, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// laidOut returns edits which apply raw, non-overlapping edits of src, the
// text of file, and lay out every top-level block they touch as the
// formatter would, where that block (or, for edits between blocks, the
// file) was formatted before (see formattedRegion). An edit replacing
// whole top-level blocks is kept as it is.
func laidOut(file string, src []byte, raw []Edit) []Edit {
	body, ok := parse(file, src)
	if !ok {
		return raw
	}
	var edits []Edit
	byRegion := map[region][]Edit{}
	regions := make([]region, 0)
	for _, e := range raw {
		var r *region
		inBlock := false
		for _, block := range body.Blocks {
			br := block.Range()
			if e.Range.Start.Byte <= br.Start.Byte && br.End.Byte <= e.Range.End.Byte {
				// the whole block goes: nothing to realign
				inBlock = true
				break
			}
			if br.Start.Byte <= e.Range.Start.Byte && e.Range.End.Byte <= br.End.Byte {
				tr := topRegion(src, block)
				r, inBlock = &tr, true
				break
			}
		}
		if !inBlock {
			r = &region{0, len(src)}
		}
		if r == nil {
			edits = append(edits, e)
			continue
		}
		if _, ok := byRegion[*r]; !ok {
			regions = append(regions, *r)
		}
		byRegion[*r] = append(byRegion[*r], e)
	}
	if whole, ok := byRegion[region{0, len(src)}]; ok && len(regions) > 1 {
		// a top-level argument goes: lay out the file once
		for _, r := range regions {
			if r != (region{0, len(src)}) {
				whole = append(whole, byRegion[r]...)
			}
		}
		return append(edits, formattedRegion(file, src, region{0, len(src)}, whole, nil)...)
	}
	for _, r := range regions {
		edits = append(edits, formattedRegion(file, src, r, byRegion[r], nil)...)
	}
	return edits
}

// sourceOf is the source text of a node.
func sourceOf(src []byte, node hclsyntax.Node) string {
	rng := node.Range()
	return string(src[rng.Start.Byte:rng.End.Byte])
}

// hasHeredoc tells whether source text of an expression holds a heredoc
// template, whose lines must stay where they are.
func hasHeredoc(text string) bool {
	tokens, _ := hclsyntax.LexExpression([]byte(text), "", hcl.InitialPos)
	for _, t := range tokens {
		if t.Type == hclsyntax.TokenOHeredoc {
			return true
		}
	}
	return false
}

// reindented moves the continuation lines of text, an expression which
// started on a line indented with from, to a line indented with to.
// Text with a heredoc keeps its lines, whose content is literal.
func reindented(text, from, to string) string {
	if !strings.Contains(text, "\n") || from == to || hasHeredoc(text) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := 0
		for n < len(line) && n < len(from) && (line[n] == ' ' || line[n] == '\t') {
			n++
		}
		lines[i] = to + line[n:]
	}
	return strings.Join(lines, "\n")
}

// localsOf returns the names of the local values the files declare.
func localsOf(files []sourceFile) map[string]bool {
	names := map[string]bool{}
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type == "locals" {
				for name := range block.Body.Attributes {
					names[name] = true
				}
			}
		}
	}
	return names
}

// jsonConfig names a JSON configuration file of the module in dir, or
// returns "" when it has none. Actions do not edit JSON files, so a
// refactoring which must find every reference refuses such a module.
func (env Env) jsonConfig(dir string) string {
	if env.ReadDir == nil {
		return ""
	}
	entries, err := env.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && !strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tofu.json")) {
			return name
		}
	}
	return ""
}

// testFiles returns the test files of the module in dir: those in dir
// and in its tests directory, as tofu test finds them.
func (env Env) testFiles(dir string) []string {
	if env.ReadDir == nil {
		return nil
	}
	var paths []string
	for _, d := range []string{dir, filepath.Join(dir, "tests")} {
		entries, err := env.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			if strings.HasSuffix(name, ".tftest.hcl") || strings.HasSuffix(name, ".tofutest.hcl") {
				paths = append(paths, filepath.Join(d, name))
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// parsedFiles reads and parses files; ok is false when one of them
// cannot be read or has syntax errors.
func (env Env) parsedFiles(paths []string) ([]sourceFile, bool) {
	files := make([]sourceFile, 0, len(paths))
	for _, path := range paths {
		src, ok := env.readFile(path)
		if !ok {
			return nil, false
		}
		body, ok := env.parse(path, src)
		if !ok {
			return nil, false
		}
		files = append(files, sourceFile{path: path, src: src, body: body})
	}
	return files, true
}

// use is a reference found in a file.
type use struct {
	file string
	src  []byte
	expr *hclsyntax.ScopeTraversalExpr
	// parent is the node holding expr.
	parent hclsyntax.Node
	// ancestors are the nodes from the attribute's value down to parent.
	ancestors []hclsyntax.Node
	// chain is the blocks around the attribute holding it.
	chain []*hclsyntax.Block
	attr  *hclsyntax.Attribute
}

func (u use) where(dir string) string {
	return fmt.Sprintf("%s:%d", filepath.ToSlash(relTo(dir, u.file)), u.expr.SrcRange.Start.Line)
}

// usesIn calls match for every reference in the attributes of the files
// and collects those it accepts. References in the provider
// meta-arguments, which name providers (provider = local.x is the
// provider local), are not visited.
func usesIn(files []sourceFile, match func(tr hcl.Traversal) bool) []use {
	var uses []use
	for _, f := range files {
		var walk func(body *hclsyntax.Body, chain []*hclsyntax.Block)
		walk = func(body *hclsyntax.Body, chain []*hclsyntax.Block) {
			names := make([]string, 0, len(body.Attributes))
			for name := range body.Attributes {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				attr := body.Attributes[name]
				if len(chain) > 0 && isProviderArgument(chain[len(chain)-1].Type, name) {
					continue
				}
				w := &stackWalker{enter: func(node hclsyntax.Node, ancestors []hclsyntax.Node) bool {
					st, ok := node.(*hclsyntax.ScopeTraversalExpr)
					if !ok || !match(st.Traversal) {
						return false
					}
					var parent hclsyntax.Node
					if n := len(ancestors); n > 0 {
						parent = ancestors[n-1]
					}
					uses = append(uses, use{
						file: f.path, src: f.src, expr: st, parent: parent,
						ancestors: append([]hclsyntax.Node(nil), ancestors...),
						chain:     chain, attr: attr,
					})
					return false
				}}
				hclsyntax.Walk(attr.Expr, w)
			}
			for _, block := range body.Blocks {
				walk(block.Body, append(append([]*hclsyntax.Block{}, chain...), block))
			}
		}
		walk(f.body, nil)
	}
	sort.SliceStable(uses, func(i, j int) bool {
		if uses[i].file != uses[j].file {
			return uses[i].file < uses[j].file
		}
		return uses[i].expr.SrcRange.Start.Byte < uses[j].expr.SrcRange.Start.Byte
	})
	return uses
}

func isProviderArgument(blockType, name string) bool {
	switch blockType {
	case "resource", "data", "ephemeral":
		return name == "provider"
	case "module":
		return name == "providers"
	}
	return false
}

// refersTo returns a matcher of the traversals which start with the steps
// root.name1.name2...
func refersTo(root string, names ...string) func(tr hcl.Traversal) bool {
	return func(tr hcl.Traversal) bool {
		if len(tr) < len(names)+1 || tr.RootName() != root {
			return false
		}
		for i, name := range names {
			attr, ok := tr[i+1].(hcl.TraverseAttr)
			if !ok || attr.Name != name {
				return false
			}
		}
		return true
	}
}

// listUses names the places of uses for a message.
func listUses(dir string, uses []use) string {
	places := make([]string, 0, len(uses))
	for _, u := range uses {
		places = append(places, u.where(dir))
	}
	if len(places) > 4 {
		return fmt.Sprintf("%s and %d more", strings.Join(places[:4], ", "), len(places)-4)
	}
	return strings.Join(places, ", ")
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	if n == 2 {
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// renameCommand is the command which starts a rename of the reference
// root.name that edits leave in the document, or nil when the client has
// no rename command.
func renameCommand(env Env, doc Document, edits []Edit, root, name string) *Command {
	if env.RenameCommand == "" || env.FileURI == nil {
		return nil
	}
	var own []Edit
	for _, e := range edits {
		if e.File == doc.Path {
			own = append(own, e)
		}
	}
	after := applyEdits(doc.Text, own)
	body, ok := parse(doc.Path, after)
	if !ok {
		return nil
	}
	var at *hcl.Pos
	hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		st, ok := node.(*hclsyntax.ScopeTraversalExpr)
		if at != nil || !ok || !refersTo(root, name)(st.Traversal) {
			return nil
		}
		// on the name, after its dot
		step := st.Traversal[1].SourceRange()
		pos := posAt(after, step.Start.Byte+1)
		at = &pos
		return nil
	})
	if at == nil {
		return nil
	}
	return &Command{
		Title:     "Rename",
		Name:      env.RenameCommand,
		Arguments: []interface{}{env.FileURI(doc.Path), ilsp.HCLPosToLSPInText(*at, after)},
	}
}

// valueText renders a string value as a quoted HCL string.
func valueText(v cty.Value) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v.AsString())
	text := strings.TrimSuffix(buf.String(), "\n")
	// JSON escapes match HCL's except for the template sequences
	text = strings.ReplaceAll(text, "${", "$${")
	text = strings.ReplaceAll(text, "%{", "%%{")
	return text
}

// blockHeaderHolds tells whether the byte b is in the header of a block:
// its type keyword or labels, before the opening brace.
func blockHeaderHolds(block *hclsyntax.Block, b int) bool {
	return block.TypeRange.Start.Byte <= b && b <= block.OpenBraceRange.Start.Byte
}

// startOfLinesAbove is the start of the comment lines right above the line
// holding b, or of that line.
func startOfLinesAbove(src []byte, b int) int {
	s := lineStart(src, b)
	for s > 0 && isComment(prevLine(src, s)) {
		s = lineStart(src, s-1)
	}
	return s
}

// countLines is the number of lines text spans.
func countLines(text string) int {
	return strings.Count(text, "\n") + 1
}
