// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// localContexts are the top-level blocks whose arguments may refer to a
// local value.
var localContexts = map[string]bool{
	"resource": true, "data": true, "ephemeral": true, "module": true,
	"output": true, "locals": true, "check": true, "provider": true,
}

// variableContexts are the top-level blocks whose arguments may refer to
// a variable: the same.
var variableContexts = localContexts

// replaceable tells whether a reference can stand where expr stands.
// Some nodes are parts of their parent's syntax rather than expressions
// of their own: the literal text of a template, an object key taken as a
// name, and the inside of a splat.
func replaceable(expr hclsyntax.Expression, parent hclsyntax.Node) bool {
	switch e := expr.(type) {
	case *hclsyntax.ObjectConsKeyExpr, *hclsyntax.AnonSymbolExpr, *hclsyntax.TemplateJoinExpr:
		return false
	case *hclsyntax.LiteralValueExpr:
		if _, ok := parent.(*hclsyntax.TemplateExpr); ok {
			return false
		}
	case *hclsyntax.RelativeTraversalExpr:
		if _, ok := e.Source.(*hclsyntax.AnonSymbolExpr); ok {
			return false
		}
	}
	if splat, ok := parent.(*hclsyntax.SplatExpr); ok && splat.Each == expr {
		return false
	}
	return true
}

// extractSite returns the expression Extract to local works on: the
// selected expression, or at a cursor the value of the innermost argument
// or object item holding it. The whole value of a local value is one
// already.
func extractSite(r *request) (*exprSite, bool) {
	var site *exprSite
	var ok bool
	if r.rng.Start.Byte == r.rng.End.Byte {
		if !r.invoked {
			return nil, false
		}
		site, ok = cursorSlot(r.body, r.rng.Start.Byte)
	} else {
		site, ok = selectedExpr(r.body, r.rng.Start.Byte, r.rng.End.Byte)
	}
	if !ok || !replaceable(site.expr, site.parent()) {
		return nil, false
	}
	if site.top().Type == "locals" && len(site.chain) == 1 && site.whole() {
		return nil, false
	}
	if _, isLocal := localName(site.expr); isLocal {
		return nil, false
	}
	return site, true
}

func extractRefusal(r *request, site *exprSite) error {
	if reason := site.contextRefusal(localContexts); reason != "" {
		return Refusal(reason)
	}
	if ref, ok := site.unseen(r.doc.Text); ok {
		return refusef("the expression refers to %s, which a local value cannot see", ref)
	}
	return nil
}

func offerExtractLocal(r *request) []Action {
	site, ok := extractSite(r)
	if !ok {
		return nil
	}
	err := extractRefusal(r, site)
	if err != nil && !r.invoked {
		// the lightbulb shows no disabled actions
		return nil
	}
	return []Action{r.offer(refExtractLocal, KindRefactorExtract, "Extract to local", err)}
}

// resolveExtractLocal moves the expression into the nearest locals block
// of the document, or a new one above its block, and replaces it with a
// reference to the new local value. A rename of the new name follows.
func resolveExtractLocal(r *request) (Action, error) {
	site, ok := extractSite(r)
	if !ok {
		return Action{}, Refusal("there is no expression to extract here")
	}
	if err := extractRefusal(r, site); err != nil {
		return Action{}, err
	}
	files, ok := moduleSources(r.env, r.doc)
	if !ok {
		return Action{}, Refusal("a file of the module has syntax errors")
	}
	name := unique(site.suggestedName(), localsOf(files))

	src, path := r.doc.Text, r.doc.Path
	rng := site.expr.Range()
	text := sourceOf(src, site.expr)
	from := indentAt(src, rng.Start.Byte)
	replacement := replace(path, src, rng.Start.Byte, rng.End.Byte, "local."+name)

	top := site.top()
	target := nearestLocals(r.body, src, top)
	if target != nil {
		open, close := target.OpenBraceRange.End.Byte, target.CloseBraceRange.Start.Byte
		if lineStart(src, open) == lineStart(src, close) && hasHeredoc(text) {
			// a heredoc cannot join { a = 1 } on one line
			target = nil
		}
	}
	var edits []Edit
	if target != nil {
		open, close := target.OpenBraceRange.End.Byte, target.CloseBraceRange.Start.Byte
		var insert Edit
		if lineStart(src, open) == lineStart(src, close) {
			// { a = 1 } on one line: spread out
			insert = insertInBody(path, src, target, []string{name + " = " + reindented(text, from, "")}, nil)[0]
		} else {
			inner := indentAt(src, target.Range().Start.Byte) + indentUnit
			at := nextLine(src, open)
			if first := firstItemStart(target.Body); first >= 0 {
				inner = indentAt(src, first)
			}
			if last := lastAttributeEnd(target.Body); last >= 0 {
				at = nextLine(src, last)
			}
			insert = replace(path, src, at, at, withNewlines(inner+name+" = "+reindented(text, from, inner)+"\n", src))
		}
		if target == top {
			edits = formattedRegion(path, src, topRegion(src, top), []Edit{replacement, insert}, nil)
		} else {
			edits = append(formattedRegion(path, src, topRegion(src, target), []Edit{insert}, nil),
				formattedRegion(path, src, topRegion(src, top), []Edit{replacement}, nil)...)
		}
	} else {
		block := "locals {\n" + indentUnit + name + " = " + reindented(text, from, indentUnit) + "\n}\n\n"
		at := startOfLinesAbove(src, top.Range().Start.Byte)
		edits = append(formattedRegion(path, src, topRegion(src, top), []Edit{replacement}, nil),
			replace(path, src, at, at, withNewlines(block, src)))
	}
	edits = mergeInsertions(edits)
	return Action{
		Title:   "Extract to local",
		Kind:    KindRefactorExtract,
		Edits:   edits,
		Command: renameCommand(r.env, r.doc, edits, "local", name),
	}, nil
}

// nearestLocals returns the locals block of the file closest to the
// top-level block top (itself when it is one), or nil.
func nearestLocals(body *hclsyntax.Body, src []byte, top *hclsyntax.Block) *hclsyntax.Block {
	if top.Type == "locals" {
		return top
	}
	var best *hclsyntax.Block
	distance := -1
	for _, block := range body.Blocks {
		if block.Type != "locals" {
			continue
		}
		d := block.Range().Start.Line - top.Range().End.Line
		if block.Range().End.Line < top.Range().Start.Line {
			d = top.Range().Start.Line - block.Range().End.Line
		}
		if distance < 0 || d < distance {
			best, distance = block, d
		}
	}
	return best
}

// mergeInsertions joins each insertion (an edit with an empty range) into
// an edit which starts or ends where it inserts, since clients order
// such edits differently.
func mergeInsertions(edits []Edit) []Edit {
	out := make([]Edit, 0, len(edits))
	merged := make([]bool, len(edits))
	for i, ins := range edits {
		if ins.Range.Start.Byte != ins.Range.End.Byte {
			continue
		}
		for j := range edits {
			if i == j || merged[j] || merged[i] || edits[j].File != ins.File || edits[j].Range.Start.Byte == edits[j].Range.End.Byte {
				continue
			}
			switch ins.Range.Start.Byte {
			case edits[j].Range.Start.Byte:
				edits[j].NewText = ins.NewText + edits[j].NewText
				merged[i] = true
			case edits[j].Range.End.Byte:
				edits[j].NewText += ins.NewText
				merged[i] = true
			}
		}
	}
	for i, e := range edits {
		if !merged[i] {
			out = append(out, e)
		}
	}
	return out
}

// isLiteral tells whether an expression is written as a constant:
// numbers, booleans, null, strings without interpolation, and lists and
// objects of those.
func isLiteral(expr hclsyntax.Expression) bool {
	switch e := expr.(type) {
	case *hclsyntax.LiteralValueExpr:
		return true
	case *hclsyntax.TemplateExpr:
		for _, part := range e.Parts {
			if _, ok := part.(*hclsyntax.LiteralValueExpr); !ok {
				return false
			}
		}
		return true
	case *hclsyntax.UnaryOpExpr:
		lit, ok := e.Val.(*hclsyntax.LiteralValueExpr)
		return ok && e.Op == hclsyntax.OpNegate && lit.Val.Type() == cty.Number
	case *hclsyntax.TupleConsExpr:
		for _, elem := range e.Exprs {
			if !isLiteral(elem) {
				return false
			}
		}
		return true
	case *hclsyntax.ObjectConsExpr:
		for _, item := range e.Items {
			key, ok := item.KeyExpr.(*hclsyntax.ObjectConsKeyExpr)
			if !ok {
				return false
			}
			if name := key.Wrapped; hclExprKeyword(name) == "" && !isLiteral(name) {
				return false
			}
			if !isLiteral(item.ValueExpr) {
				return false
			}
		}
		return true
	}
	return false
}

func hclExprKeyword(expr hclsyntax.Expression) string {
	st, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(st.Traversal) != 1 {
		return ""
	}
	return st.Traversal.RootName()
}

// literalSite returns the literal Introduce variable works on: the
// selected literal, or at a cursor the outermost literal holding it
// within the innermost argument or object item.
func literalSite(r *request) (*exprSite, bool) {
	if r.rng.Start.Byte != r.rng.End.Byte {
		site, ok := selectedExpr(r.body, r.rng.Start.Byte, r.rng.End.Byte)
		if !ok || !replaceable(site.expr, site.parent()) || !isLiteral(site.expr) {
			return nil, false
		}
		return site, true
	}
	if !r.invoked {
		return nil, false
	}
	slot, ok := cursorSlot(r.body, r.rng.Start.Byte)
	if !ok {
		return nil, false
	}
	within := slot.expr.Range()
	for _, node := range pathAt(slot.attr, r.rng.Start.Byte) {
		expr, ok := node.(hclsyntax.Expression)
		if !ok || !holds(within, expr.Range().Start.Byte, expr.Range().End.Byte) || !isLiteral(expr) {
			continue
		}
		site := siteOf(slot.chain, slot.attr, expr)
		if site == nil || !replaceable(site.expr, site.parent()) {
			continue
		}
		return site, true
	}
	return nil, false
}

// literalType returns the type to declare for a variable holding the
// literal, so that the reference has the value the literal had: the
// argument's type where the literal is a whole argument of a known type
// (the value converts to it either way), else the literal's own type.
// A list or an object literal is a tuple or an object, which a list or
// a map variable would change where the argument takes any type, such
// as terraform_data's input.
func literalType(r *request, site *exprSite) (cty.Type, error) {
	v, diags := site.expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilType, Refusal("the value is not valid")
	}
	if v.IsNull() {
		return cty.NilType, Refusal("null has no type to declare")
	}
	ty := v.Type()
	if ty.IsPrimitiveType() {
		return ty, nil
	}
	if site.whole() {
		if at, ok := argumentType(r.env, r.doc.dir(), site.chain, site.attr.Name); ok && !at.HasDynamicTypes() {
			if _, err := convert.Convert(v, at); err == nil {
				return at, nil
			}
		}
	}
	if ty.HasDynamicTypes() {
		return cty.NilType, Refusal("the value holds null, which has no type to declare")
	}
	return ty, nil
}

func offerIntroduceVariable(r *request) []Action {
	site, ok := literalSite(r)
	if !ok {
		return nil
	}
	var err error
	if reason := site.contextRefusal(variableContexts); reason != "" {
		err = Refusal(reason)
	} else if _, typeErr := literalType(r, site); typeErr != nil {
		err = typeErr
	}
	if err != nil && !r.invoked {
		return nil
	}
	actions := []Action{r.offer(refIntroduceVariable, KindRefactorExtract, "Introduce variable", err)}
	if r.env.CreateFiles || (r.invoked && r.env.fileExists(filepath.Join(r.doc.dir(), "terraform.tfvars"))) {
		if err == nil && r.invoked && r.env.IndexedCallers != nil {
			// the callers known so far; resolving it looks for all of them
			if callers, _ := r.callers(); len(callers) > 0 {
				err = calledRefusal(r.doc.dir(), callers[0])
			}
		}
		actions = append(actions, r.offer(refIntroduceTfvars, KindRefactorExtract, "Introduce variable set in terraform.tfvars", err))
	}
	return actions
}

// calledRefusal says that the tfvars variant does not fit a child module.
func calledRefusal(dir string, c ModuleCaller) error {
	return refusef("module.%s (in %s) calls this module, and terraform.tfvars sets only the variables of a root module", c.Name, filepath.ToSlash(relTo(dir, c.Dir)))
}

func (env Env) fileExists(path string) bool {
	_, ok := env.readFile(path)
	return ok
}

// resolveIntroduceVariable declares a variable whose default is the
// literal, or without a default and with the literal set in
// terraform.tfvars, and replaces the literal with a reference to it. A
// rename of the new name follows.
func resolveIntroduceVariable(r *request, tfvars bool) (Action, error) {
	site, ok := literalSite(r)
	if !ok {
		return Action{}, Refusal("there is no literal here")
	}
	if reason := site.contextRefusal(variableContexts); reason != "" {
		return Action{}, Refusal(reason)
	}
	ty, err := literalType(r, site)
	if err != nil {
		return Action{}, err
	}
	dir := r.doc.dir()
	if tfvars {
		callers, ok := r.callers()
		if !ok {
			return Action{}, Refusal("the modules which call this module could not be read")
		}
		if len(callers) > 0 {
			return Action{}, calledRefusal(dir, callers[0])
		}
	}
	files, ok := moduleSources(r.env, r.doc)
	if !ok {
		return Action{}, Refusal("a file of the module has syntax errors")
	}
	taken := map[string]bool{}
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type == "variable" && len(block.Labels) == 1 {
				taken[block.Labels[0]] = true
			}
		}
	}
	// a tfvars key without a declaration would set the new variable
	for name := range r.env.varsFileKeys(dir) {
		taken[name] = true
	}
	name := unique(site.suggestedName(), taken)

	src := r.doc.Text
	rng := site.expr.Range()
	text := sourceOf(src, site.expr)
	from := indentAt(src, rng.Start.Byte)
	edits := formattedRegion(r.doc.Path, src, topRegion(src, site.top()), []Edit{
		replace(r.doc.Path, src, rng.Start.Byte, rng.End.Byte, "var."+name),
	}, nil)

	decl := fmt.Sprintf("variable %q {\n  type    = %s\n  default = %s\n}\n", name, typeexpr.TypeString(ty), reindented(text, from, indentUnit))
	if tfvars {
		decl = fmt.Sprintf("variable %q {\n  type = %s\n}\n", name, typeexpr.TypeString(ty))
	}
	if !hasHeredoc(decl) {
		decl = string(hclwrite.Format([]byte(decl)))
	}
	target, create := variablesFile(r.env, r.doc)
	if target == "" {
		return Action{}, Refusal("there is no file to declare the variable in")
	}
	a := Action{Kind: KindRefactorExtract, Title: "Introduce variable"}
	switch {
	case create:
		a.Create = append(a.Create, target)
		edits = append(edits, replace(target, nil, 0, 0, decl))
	case target == r.doc.Path:
		edits = append(edits, appendBlock(target, src, decl))
	default:
		vsrc, ok := r.env.readFile(target)
		if !ok {
			return Action{}, refusef("%s cannot be read", filepath.Base(target))
		}
		edits = append(edits, appendBlock(target, vsrc, decl))
	}

	if tfvars {
		a.Title = "Introduce variable set in terraform.tfvars"
		path := filepath.Join(dir, "terraform.tfvars")
		setting := name + " = " + reindented(text, from, "") + "\n"
		vsrc, exists := r.env.readFile(path)
		switch {
		case exists:
			if _, ok := parse(path, vsrc); !ok {
				return Action{}, Refusal("terraform.tfvars has syntax errors")
			}
			prefix := ""
			if len(vsrc) > 0 && vsrc[len(vsrc)-1] != '\n' {
				prefix = "\n"
			}
			edits = append(edits, replace(path, vsrc, len(vsrc), len(vsrc), withNewlines(prefix+setting, vsrc)))
		case r.env.CreateFiles:
			a.Create = append(a.Create, path)
			edits = append(edits, replace(path, nil, 0, 0, setting))
		default:
			return Action{}, Refusal("terraform.tfvars does not exist, and the editor cannot create it")
		}
	}
	a.Edits = mergeInsertions(edits)
	a.Command = renameCommand(r.env, r.doc, a.Edits, "var", name)
	return a, nil
}

// varsFileKeys returns the names that the tfvars files which OpenTofu
// loads automatically in dir set.
func (env Env) varsFileKeys(dir string) map[string]bool {
	keys := map[string]bool{}
	if env.ReadDir == nil {
		return keys
	}
	entries, err := env.ReadDir(dir)
	if err != nil {
		return keys
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		switch {
		case name == "terraform.tfvars" || strings.HasSuffix(name, ".auto.tfvars"):
			src, ok := env.readFile(path)
			if !ok {
				continue
			}
			body, ok := parse(path, src)
			if !ok {
				continue
			}
			for key := range body.Attributes {
				keys[key] = true
			}
		case name == "terraform.tfvars.json" || strings.HasSuffix(name, ".auto.tfvars.json"):
			src, ok := env.readFile(path)
			if !ok {
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(bytes.TrimSpace(src), &m) == nil {
				for key := range m {
					keys[key] = true
				}
			}
		}
	}
	return keys
}
