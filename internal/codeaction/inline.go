// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// maxInlineLines is the length of a local value's expression above which
// it is inlined everywhere only when it is used once: copies of a long
// expression are harder to read than the name.
const maxInlineLines = 3

// localAt returns the name of the local value at the cursor: the name in
// its declaration, or a use local.x (then also the use).
func localAt(r *request) (string, *hclsyntax.ScopeTraversalExpr, bool) {
	b := r.rng.Start.Byte
	for _, block := range r.body.Blocks {
		if block.Type != "locals" || !within(block.Range(), b) {
			continue
		}
		for name, attr := range block.Body.Attributes {
			if within(attr.NameRange, b) {
				return name, nil, true
			}
		}
	}
	chain, attr, ok := attributeAt(r.body, b, b)
	if !ok || isProviderArgument(chain[len(chain)-1].Type, attr.Name) {
		return "", nil, false
	}
	path := pathAt(attr, b)
	for i := len(path) - 1; i >= 0; i-- {
		st, ok := path[i].(*hclsyntax.ScopeTraversalExpr)
		if !ok {
			continue
		}
		name, ok := localStep(st.Traversal)
		if ok && b <= st.Traversal[1].SourceRange().End.Byte {
			return name, st, true
		}
		return "", nil, false
	}
	return "", nil, false
}

// localStep returns x for a traversal local.x, with or without further
// steps.
func localStep(tr hcl.Traversal) (string, bool) {
	if len(tr) < 2 || tr.RootName() != "local" {
		return "", false
	}
	attr, ok := tr[1].(hcl.TraverseAttr)
	return attr.Name, ok
}

// inlining is what inlining a local value needs.
type inlining struct {
	name string
	decl *hclsyntax.Attribute
	// declFile holds the declaration.
	declFile sourceFile
	// block is the locals block of the declaration.
	block *hclsyntax.Block
	text  string
	uses  []use
	files []sourceFile
}

// analyzeInline finds the declaration and the uses of the local value
// name. The error says why it cannot be inlined anywhere.
func analyzeInline(r *request, name string) (*inlining, error) {
	dir := r.doc.dir()
	if json := r.env.jsonConfig(dir); json != "" {
		return nil, refusef("%s cannot be edited, and may use local.%s", json, name)
	}
	files, ok := moduleSources(r.env, r.doc)
	if !ok {
		return nil, Refusal("a file of the module has syntax errors")
	}
	in := &inlining{name: name, files: files}
	var declared []string
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type != "locals" {
				continue
			}
			if attr, ok := block.Body.Attributes[name]; ok {
				in.decl, in.declFile, in.block = attr, f, block
				declared = append(declared, filepath.ToSlash(relTo(dir, f.path)))
			}
		}
	}
	switch {
	case len(declared) == 0:
		return nil, refusef("local.%s is not declared in this module", name)
	case len(declared) > 1:
		return nil, refusef("local.%s is declared more than once (%s)", name, strings.Join(declared, ", "))
	}
	in.text = sourceOf(in.declFile.src, in.decl.Expr)
	for _, u := range usesIn(files, refersTo("local", name)) {
		if u.attr == in.decl {
			continue
		}
		in.uses = append(in.uses, u)
	}
	return in, nil
}

// useRefusal says why the expression cannot replace the use, or
// returns "".
func (in *inlining) useRefusal(dir string, u use) string {
	scope := map[string]bool{}
	site := &exprSite{expr: u.expr, ancestors: u.ancestors, chain: u.chain, attr: u.attr}
	for name := range site.scopeNames() {
		scope[name] = true
	}
	for _, tr := range in.decl.Expr.Variables() {
		if scope[tr.RootName()] {
			return fmt.Sprintf("at %s, the name %s is bound by the surrounding expression", u.where(dir), tr.RootName())
		}
	}
	if hasHeredoc(in.text) && !(len(u.ancestors) == 0 && len(u.expr.Traversal) == 2) {
		return fmt.Sprintf("local.%s is a heredoc, which can replace only a whole argument (not at %s)", in.name, u.where(dir))
	}
	return ""
}

func offerInlineLocal(r *request) []Action {
	name, useExpr, ok := localAt(r)
	if !ok {
		return nil
	}
	var actions []Action
	in, err := analyzeInline(r, name)
	if useExpr != nil {
		hereErr := err
		if err == nil {
			if u, ok := in.useOf(r.doc.Path, useExpr); ok {
				if reason := in.useRefusal(r.doc.dir(), u); reason != "" {
					hereErr = Refusal(reason)
				}
			}
		}
		actions = append(actions, r.offer(refInlineLocalHere, KindRefactorInline, fmt.Sprintf("Inline this use of local %q", name), hereErr))
	}
	if err == nil {
		err = in.allRefusal(r)
	}
	return append(actions, r.offer(refInlineLocal, KindRefactorInline, fmt.Sprintf("Inline local %q", name), err))
}

// useOf returns the use of the local value whose expression is expr, a
// node of the document (which the module's files may hold as another
// parse).
func (in *inlining) useOf(file string, expr *hclsyntax.ScopeTraversalExpr) (use, bool) {
	for _, u := range in.uses {
		if u.file == file && u.expr.SrcRange.Start.Byte == expr.SrcRange.Start.Byte && u.expr.SrcRange.End.Byte == expr.SrcRange.End.Byte {
			return u, true
		}
	}
	return use{}, false
}

// allRefusal says why every use cannot be inlined, the local value
// removed.
func (in *inlining) allRefusal(r *request) error {
	dir := r.doc.dir()
	if lines := countLines(in.text); lines > maxInlineLines && len(in.uses) > 1 {
		return refusef("local.%s is %d lines long and used %s; inline its uses one at a time", in.name, lines, times(len(in.uses)))
	}
	for _, u := range in.uses {
		if reason := in.useRefusal(dir, u); reason != "" {
			return Refusal(reason)
		}
	}
	tests, ok := r.env.parsedFiles(r.env.testFiles(dir))
	if !ok {
		return Refusal("a test file of the module has syntax errors")
	}
	if uses := usesIn(tests, refersTo("local", in.name)); len(uses) > 0 {
		return refusef("a test file refers to local.%s (%s)", in.name, listUses(dir, uses))
	}
	return nil
}

// resolveInlineLocal replaces the use at the cursor (here), or every use
// of the local value and then its declaration, with its expression.
func resolveInlineLocal(r *request, here bool) (Action, error) {
	name, useExpr, ok := localAt(r)
	if !ok || (here && useExpr == nil) {
		return Action{}, Refusal("there is no local value here")
	}
	in, err := analyzeInline(r, name)
	if err != nil {
		return Action{}, err
	}
	dir := r.doc.dir()
	uses := in.uses
	title := fmt.Sprintf("Inline local %q", name)
	if here {
		u, ok := in.useOf(r.doc.Path, useExpr)
		if !ok {
			return Action{}, Refusal("the use was not found")
		}
		if reason := in.useRefusal(dir, u); reason != "" {
			return Action{}, Refusal(reason)
		}
		uses = []use{u}
		title = fmt.Sprintf("Inline this use of local %q", name)
	} else if err := in.allRefusal(r); err != nil {
		return Action{}, err
	}

	raw := map[string][]Edit{}
	srcs := map[string][]byte{}
	order := make([]string, 0)
	add := func(file string, src []byte, e Edit) {
		if _, ok := srcs[file]; !ok {
			srcs[file] = src
			order = append(order, file)
		}
		raw[file] = append(raw[file], e)
	}
	for _, u := range uses {
		add(u.file, u.src, in.replacement(u))
	}
	if !here {
		f := in.declFile
		start, end := in.decl.SrcRange.Start.Byte, in.decl.SrcRange.End.Byte
		if len(in.block.Body.Attributes) == 1 && len(in.block.Body.Blocks) == 0 {
			start, end = in.block.Range().Start.Byte, in.block.Range().End.Byte
		}
		add(f.path, f.src, deleteElement(f.path, f.src, start, end))
	}
	var edits []Edit
	for _, file := range order {
		edits = append(edits, laidOut(file, srcs[file], raw[file])...)
	}
	return Action{Title: title, Kind: KindRefactorInline, Edits: edits}, nil
}

// replacement is the edit which replaces the use with the expression:
// parenthesized where the use's surroundings would otherwise bind parts
// of it, and spliced into a quoted template when both are one.
func (in *inlining) replacement(u use) Edit {
	expr := in.decl.Expr
	text := reindented(in.text, indentAt(in.declFile.src, expr.Range().Start.Byte), indentAt(u.src, u.expr.SrcRange.Start.Byte))
	tr := u.expr.Traversal
	start := u.expr.SrcRange.Start.Byte
	end := tr[1].SourceRange().End.Byte
	more := len(tr) > 2

	if !more {
		if s, e, ok := interpolationAround(u.src, u.expr, u.parent); ok && isQuotedTemplate(in.declFile.src, expr) {
			inner := text[1 : len(text)-1]
			// "a$" before "{b}" would start an interpolation
			opens := strings.HasSuffix(inner, "$") || strings.HasSuffix(inner, "%")
			if !opens || e >= len(u.src) || u.src[e] != '{' {
				return replace(u.file, u.src, s, e, inner)
			}
		}
	}
	if inlineNeedsParens(expr, u.expr, u.parent, more) {
		text = "(" + text + ")"
	}
	return replace(u.file, u.src, start, end, text)
}

// inlineNeedsParens tells whether expr must be parenthesized to replace
// the use, a reference with further steps (more) or not.
func inlineNeedsParens(expr hclsyntax.Expression, useExpr hclsyntax.Expression, parent hclsyntax.Node, more bool) bool {
	indexed := more
	switch p := parent.(type) {
	case *hclsyntax.IndexExpr:
		indexed = indexed || p.Collection == useExpr
	case *hclsyntax.SplatExpr:
		indexed = indexed || p.Source == useExpr
	case *hclsyntax.RelativeTraversalExpr:
		indexed = indexed || p.Source == useExpr
	}
	if indexed {
		switch expr.(type) {
		case *hclsyntax.ScopeTraversalExpr, *hclsyntax.FunctionCallExpr, *hclsyntax.ParenthesesExpr, *hclsyntax.IndexExpr:
			return false
		}
		return true
	}
	return needsParens(expr, parent)
}

// isQuotedTemplate tells whether expr is a template in quotes (not a
// heredoc), whose text inside the quotes can join another quoted
// template.
func isQuotedTemplate(src []byte, expr hclsyntax.Expression) bool {
	switch expr.(type) {
	case *hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr:
	default:
		return false
	}
	text := sourceOf(src, expr)
	return len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"'
}

// interpolationAround returns the interpolation ${ ... } holding only the
// use, a part of a quoted template, when it has no strip markers.
func interpolationAround(src []byte, useExpr *hclsyntax.ScopeTraversalExpr, parent hclsyntax.Node) (int, int, bool) {
	var tmpl hclsyntax.Node
	switch p := parent.(type) {
	case *hclsyntax.TemplateExpr:
		tmpl = p
	case *hclsyntax.TemplateWrapExpr:
		tmpl = p
	default:
		return 0, 0, false
	}
	if t := sourceOf(src, tmpl); len(t) == 0 || t[0] != '"' {
		return 0, 0, false
	}
	s := useExpr.SrcRange.Start.Byte
	for s > 0 && (src[s-1] == ' ' || src[s-1] == '\t') {
		s--
	}
	if s < 2 || src[s-1] != '{' || src[s-2] != '$' {
		return 0, 0, false
	}
	e := useExpr.SrcRange.End.Byte
	for e < len(src) && (src[e] == ' ' || src[e] == '\t') {
		e++
	}
	if e >= len(src) || src[e] != '}' {
		return 0, 0, false
	}
	return s - 2, e + 1, true
}
