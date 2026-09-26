// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// declareMissing declares the variable or local value an unresolved
// var.x or local.x refers to.
func declareMissing(env Env, doc Document, diag Diagnostic) []Action {
	if doc.Vars {
		return nil
	}
	addr := diag.dataString("address")
	if addr == "" && diag.Range.End.Byte <= len(doc.Text) {
		addr = string(doc.Text[diag.Range.Start.Byte:diag.Range.End.Byte])
	}
	tr, diags := hclsyntax.ParseTraversalAbs([]byte(addr), "", hcl.InitialPos)
	if diags.HasErrors() || len(tr) < 2 {
		return nil
	}
	attr, ok := tr[1].(hcl.TraverseAttr)
	if !ok {
		return nil
	}
	switch tr.RootName() {
	case "var":
		return declareVariable(env, doc, attr.Name, typeOfUse(env, doc, diag))
	case "local":
		return declareLocal(env, doc, attr.Name)
	}
	return nil
}

// declareTfvarsVariable declares a variable that a var file sets, with
// the type of the value it sets.
func declareTfvarsVariable(env Env, doc Document, diag Diagnostic) []Action {
	name := diag.dataString("name")
	var value hcl.Expression
	if f, diags := hclsyntax.ParseConfig(doc.Text, doc.Path, hcl.InitialPos); !diags.HasErrors() {
		if attrs, diags := f.Body.JustAttributes(); !diags.HasErrors() {
			for _, attr := range attrs {
				if (name != "" && attr.Name == name) || (name == "" && touches(attr.NameRange, diag.Range)) {
					name, value = attr.Name, attr.Expr
				}
			}
		}
	}
	if !hclsyntax.ValidIdentifier(name) {
		return nil
	}

	ty := cty.NilType
	if s := diag.dataString("valueType"); s != "" {
		if expr, diags := hclsyntax.ParseExpression([]byte(s), "", hcl.InitialPos); !diags.HasErrors() {
			if t, diags := typeexpr.TypeConstraint(expr); !diags.HasErrors() {
				ty = t
			}
		}
	} else if value != nil {
		if v, diags := value.Value(nil); !diags.HasErrors() && v.IsWhollyKnown() && !v.IsNull() {
			ty = v.Type()
		}
	}
	if g, ok := generalized(ty); ok {
		ty = g
	} else {
		ty = cty.NilType
	}
	return declareVariable(env, doc, name, ty)
}

// typeOfUse returns the type of the argument whose value is exactly the
// unresolved reference, such as the string type for name = var.x in a
// resource whose name argument is a string.
func typeOfUse(env Env, doc Document, diag Diagnostic) cty.Type {
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return cty.NilType
	}
	var found cty.Type = cty.NilType
	var walk func(body *hclsyntax.Body, chain []*hclsyntax.Block)
	walk = func(body *hclsyntax.Body, chain []*hclsyntax.Block) {
		for name, attr := range body.Attributes {
			st, ok := attr.Expr.(*hclsyntax.ScopeTraversalExpr)
			if !ok || len(st.Traversal) != 2 || !touches(st.SrcRange, diag.Range) {
				continue
			}
			if ty, ok := argumentType(env, doc.dir(), chain, name); ok {
				found = ty
			}
		}
		for _, block := range body.Blocks {
			if block.Range().Start.Byte <= diag.Range.Start.Byte && diag.Range.End.Byte <= block.Range().End.Byte {
				walk(block.Body, append(append([]*hclsyntax.Block{}, chain...), block))
			}
		}
	}
	walk(body, nil)
	return found
}

// declareVariable declares a variable in the module of the document:
// in variables.tf if there is one, else in the file holding the most
// variable blocks, else in a new variables.tf.
func declareVariable(env Env, doc Document, name string, ty cty.Type) []Action {
	dir := doc.dir()
	target, create := variablesFile(env, doc)
	if target == "" {
		return nil
	}

	text := fmt.Sprintf("variable %q {}\n", name)
	if t := declaredTypeString(ty); t != "" {
		text = string(hclwrite.Format([]byte(fmt.Sprintf("variable %q {\n  type = %s\n}\n", name, t))))
	}

	rel := filepath.ToSlash(relTo(dir, target))
	action := Action{Preferred: true}
	if create {
		action.Title = fmt.Sprintf("Declare variable %q in a new %s", name, rel)
		action.Create = []string{target}
		action.Edits = []Edit{replace(target, nil, 0, 0, text)}
		return []Action{action}
	}
	src := doc.Text
	if target != doc.Path {
		var ok bool
		if src, ok = env.readFile(target); !ok {
			return nil
		}
	}
	action.Title = fmt.Sprintf("Declare variable %q in %s", name, rel)
	action.Edits = []Edit{appendBlock(target, src, text)}
	return []Action{action}
}

// variablesFile chooses the file of the document's module to declare a
// variable in. create is true when the file does not exist yet.
func variablesFile(env Env, doc Document) (string, bool) {
	dir := doc.dir()
	candidate := filepath.Join(dir, "variables.tf")
	if _, ok := env.readFile(candidate); ok {
		return candidate, false
	}

	best, most := "", 0
	files := env.moduleFiles(dir)
	for _, path := range files {
		src := doc.Text
		if path != doc.Path {
			var ok bool
			if src, ok = env.readFile(path); !ok {
				continue
			}
		}
		body, ok := env.parse(path, src)
		if !ok {
			continue
		}
		n := 0
		for _, block := range body.Blocks {
			if block.Type == "variable" {
				n++
			}
		}
		if n > most {
			best, most = path, n
		}
	}
	switch {
	case best != "":
		return best, false
	case env.CreateFiles:
		return candidate, true
	case !doc.Vars:
		return doc.Path, false
	case len(files) > 0:
		return files[0], false
	}
	return "", false
}

func relTo(dir, path string) string {
	if rel, err := filepath.Rel(dir, path); err == nil {
		return rel
	}
	return path
}

// declareLocal adds the local value, set to null with a TODO, to the
// first locals block of the document (else of the module), or to a new
// locals block at the end of the document. The value is a guess, so the
// fix is never the preferred one.
func declareLocal(env Env, doc Document, name string) []Action {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	item := fmt.Sprintf("%s = null # TODO", name)
	title := fmt.Sprintf("Declare local %q", name)
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type != "locals" {
				continue
			}
			edits := formattedBlock(f.path, f.src, block, block, insertInBody(f.path, f.src, block, []string{item}, nil))
			if f.path != doc.Path {
				title += " in " + filepath.ToSlash(relTo(doc.dir(), f.path))
			}
			return []Action{{Title: title, Edits: edits}}
		}
	}
	text := "locals {\n" + indentUnit + item + "\n}\n"
	return []Action{{Title: title, Edits: []Edit{appendBlock(doc.Path, doc.Text, text)}}}
}

// quoted shortens source text for a title.
func quoted(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > max {
		return text[:max-1] + "…"
	}
	return text
}
