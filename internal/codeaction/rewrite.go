// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// maxLocalDepth limits how many locals are followed to find what a
// local holds.
const maxLocalDepth = 3

func within(rng hcl.Range, b int) bool {
	return rng.Start.Byte <= b && b <= rng.End.Byte
}

// rewriteCall offers to rewrite the innermost element() or lookup() call
// at pos as an index expression. element() wraps around (the index is
// taken modulo the length), so its rewrite is offered only where the
// index is provably within the list; elsewhere the action is disabled
// with the reason.
func rewriteCall(env Env, doc Document, body *hclsyntax.Body, pos hcl.Pos) []Action {
	var top *hclsyntax.Block
	for _, block := range body.Blocks {
		if within(block.Range(), pos.Byte) {
			top = block
		}
	}
	if top == nil {
		return nil
	}
	var call *hclsyntax.FunctionCallExpr
	hclsyntax.VisitAll(top.Body, func(node hclsyntax.Node) hcl.Diagnostics {
		fc, ok := node.(*hclsyntax.FunctionCallExpr)
		if !ok || (fc.Name != "element" && fc.Name != "lookup") || !within(fc.Range(), pos.Byte) {
			return nil
		}
		if call == nil || (call.Range().Start.Byte <= fc.Range().Start.Byte && fc.Range().End.Byte <= call.Range().End.Byte) {
			call = fc
		}
		return nil
	})
	if call == nil || len(call.Args) != 2 || call.ExpandFinal {
		return nil
	}

	src := func(e hclsyntax.Expression) string {
		return string(doc.Text[e.Range().Start.Byte:e.Range().End.Byte])
	}
	coll, key := call.Args[0], call.Args[1]
	collText := src(coll)
	if !isPrimary(coll) {
		collText = "(" + collText + ")"
	}
	result := collText + "[" + src(key) + "]"
	edit := replace(doc.Path, doc.Text, call.Range().Start.Byte, call.Range().End.Byte, result)

	if call.Name == "lookup" {
		// without a default, lookup fails on a missing key just like an
		// index does
		return []Action{{
			Title:     "Replace lookup() with " + quoted(result, 50),
			Preferred: true,
			Edits:     []Edit{edit},
		}}
	}

	if reason := elementUnsafe(env, doc, top, coll, key); reason != "" {
		return []Action{{Title: "Replace element() with an index", Disabled: reason}}
	}
	return []Action{{
		Title:     "Replace element() with " + quoted(result, 50),
		Preferred: true,
		Edits:     []Edit{edit},
	}}
}

// elementUnsafe says why element(coll, key) cannot be written coll[key]
// with the same meaning, or returns "" when it can: the key is a literal
// smaller than the statically known length of coll, or count.index in a
// block whose count is length(coll) of a list.
func elementUnsafe(env Env, doc Document, top *hclsyntax.Block, coll, key hclsyntax.Expression) string {
	if n, ok := literalIndex(key); ok {
		length, ok := staticLength(env, doc, coll, 0)
		if !ok {
			return "element() wraps around, and the length of the list is not known statically"
		}
		if n >= length {
			return fmt.Sprintf("element() wraps around: index %d is past the end of the list of %d", n, length)
		}
		return ""
	}
	if isCountIndex(key) {
		count, ok := top.Body.Attributes["count"]
		if !ok {
			return "count.index is not bounded by a count here"
		}
		if !countBounded(doc.Text, count.Expr, coll) {
			return "element() wraps around, and count is not length() of the same list"
		}
		if !listTyped(env, doc, coll, 0) {
			return "the collection is not known to be a list, which an index needs"
		}
		return ""
	}
	return "element() wraps around, and the index is not provably within the list"
}

// countBounded tells whether count is at most the length of coll:
// length(coll), or a conditional choosing between that and 0, as in
// length(var.l) > 0 ? length(var.l) : 0.
func countBounded(src []byte, count, coll hclsyntax.Expression) bool {
	switch e := count.(type) {
	case *hclsyntax.ParenthesesExpr:
		return countBounded(src, e.Expression, coll)
	case *hclsyntax.ConditionalExpr:
		return countBounded(src, e.TrueResult, coll) && countBounded(src, e.FalseResult, coll)
	case *hclsyntax.LiteralValueExpr:
		return e.Val.Type() == cty.Number && !e.Val.IsNull() && e.Val.Equals(cty.Zero).True()
	case *hclsyntax.FunctionCallExpr:
		return e.Name == "length" && len(e.Args) == 1 && !e.ExpandFinal && compact(src, e.Args[0]) == compact(src, coll)
	}
	return false
}

// literalIndex returns the value of a literal whole number that is not
// negative.
func literalIndex(expr hclsyntax.Expression) (int, bool) {
	lit, ok := expr.(*hclsyntax.LiteralValueExpr)
	if !ok || lit.Val.Type() != cty.Number || lit.Val.IsNull() {
		return 0, false
	}
	bf := lit.Val.AsBigFloat()
	if !bf.IsInt() || bf.Sign() < 0 {
		return 0, false
	}
	i, acc := bf.Int64()
	if acc != 0 || i > 1<<31 {
		return 0, false
	}
	return int(i), true
}

func isCountIndex(expr hclsyntax.Expression) bool {
	st, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(st.Traversal) != 2 || st.Traversal.RootName() != "count" {
		return false
	}
	attr, ok := st.Traversal[1].(hcl.TraverseAttr)
	return ok && attr.Name == "index"
}

// compact is the source of an expression without whitespace, to compare
// two expressions written differently.
func compact(src []byte, expr hclsyntax.Expression) string {
	text := string(src[expr.Range().Start.Byte:expr.Range().End.Byte])
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
}

// localValue returns the expression of the module's local value name,
// with the source of its file.
func localValue(env Env, doc Document, name string) (hclsyntax.Expression, []byte, bool) {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil, nil, false
	}
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type != "locals" {
				continue
			}
			if attr, ok := block.Body.Attributes[name]; ok {
				return attr.Expr, f.src, true
			}
		}
	}
	return nil, nil, false
}

// localName returns x for the expression local.x.
func localName(expr hclsyntax.Expression) (string, bool) {
	st, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(st.Traversal) != 2 || st.Traversal.RootName() != "local" {
		return "", false
	}
	attr, ok := st.Traversal[1].(hcl.TraverseAttr)
	return attr.Name, ok
}

// staticLength is the number of elements of a tuple literal, directly or
// through local values. Its elements may be anything; their number is
// fixed.
func staticLength(env Env, doc Document, expr hclsyntax.Expression, depth int) (int, bool) {
	switch e := expr.(type) {
	case *hclsyntax.TupleConsExpr:
		return len(e.Exprs), true
	case *hclsyntax.ParenthesesExpr:
		return staticLength(env, doc, e.Expression, depth)
	}
	if name, ok := localName(expr); ok && depth < maxLocalDepth {
		if value, _, ok := localValue(env, doc, name); ok {
			return staticLength(env, doc, value, depth+1)
		}
	}
	return 0, false
}

// listFunctions return a list or a tuple whenever they succeed.
var listFunctions = map[string]bool{
	"chunklist":  true,
	"compact":    true,
	"concat":     true,
	"distinct":   true,
	"flatten":    true,
	"formatlist": true,
	"keys":       true,
	"matchkeys":  true,
	"range":      true,
	"reverse":    true,
	"slice":      true,
	"sort":       true,
	"split":      true,
	"tolist":     true,
	"values":     true,
}

// listTyped tells whether an expression certainly has a list or tuple
// value (which an index can address), rather than a set or a map.
func listTyped(env Env, doc Document, expr hclsyntax.Expression, depth int) bool {
	switch e := expr.(type) {
	case *hclsyntax.TupleConsExpr, *hclsyntax.SplatExpr:
		return true
	case *hclsyntax.ForExpr:
		return e.KeyExpr == nil
	case *hclsyntax.FunctionCallExpr:
		return listFunctions[e.Name]
	case *hclsyntax.ParenthesesExpr:
		return listTyped(env, doc, e.Expression, depth)
	case *hclsyntax.ConditionalExpr:
		return listTyped(env, doc, e.TrueResult, depth) && listTyped(env, doc, e.FalseResult, depth)
	case *hclsyntax.ScopeTraversalExpr:
		if len(e.Traversal) != 2 {
			return false
		}
		attr, ok := e.Traversal[1].(hcl.TraverseAttr)
		if !ok {
			return false
		}
		switch e.Traversal.RootName() {
		case "var":
			if env.VariableType == nil {
				return false
			}
			ty, ok := env.VariableType(doc.dir(), attr.Name)
			return ok && (ty.IsListType() || ty.IsTupleType())
		case "local":
			if depth >= maxLocalDepth {
				return false
			}
			if value, _, ok := localValue(env, doc, attr.Name); ok {
				return listTyped(env, doc, value, depth+1)
			}
		}
	}
	return false
}

// runInit offers the client's init command for the module directory of
// a module or provider that is not installed.
func runInit(env Env, doc Document, diag Diagnostic) []Action {
	if env.InitCommand == "" || env.DirURI == nil {
		return nil
	}
	dir := diag.dataString("dir")
	if dir == "" {
		dir = doc.dir()
	}
	title := "Run tofu init"
	return []Action{{
		Title:     title,
		Preferred: true,
		Command:   &Command{Title: title, Name: env.InitCommand, Arguments: []interface{}{env.DirURI(dir)}},
	}}
}
