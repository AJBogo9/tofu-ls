// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// parentWalker visits every node with its parent.
type parentWalker struct {
	stack []hclsyntax.Node
	visit func(node, parent hclsyntax.Node)
}

func (w *parentWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	var parent hclsyntax.Node
	if n := len(w.stack); n > 0 {
		parent = w.stack[n-1]
	}
	w.stack = append(w.stack, node)
	w.visit(node, parent)
	return nil
}

func (w *parentWalker) Exit(node hclsyntax.Node) hcl.Diagnostics {
	w.stack = w.stack[:len(w.stack)-1]
	return nil
}

// unwrapInterpolation replaces "${expr}" with expr. Where the template
// was an operand, a non-trivial expression is parenthesized to keep its
// precedence, and an object key is always parenthesized, so that a bare
// name is not taken as a literal key.
func unwrapInterpolation(env Env, doc Document, diag Diagnostic) []Action {
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return nil
	}
	var target *hclsyntax.TemplateWrapExpr
	var parent hclsyntax.Node
	w := &parentWalker{visit: func(node, p hclsyntax.Node) {
		wrap, ok := node.(*hclsyntax.TemplateWrapExpr)
		if !ok {
			return
		}
		rng := wrap.SrcRange
		exact := rng.Start.Byte == diag.Range.Start.Byte && rng.End.Byte == diag.Range.End.Byte
		if exact || (target == nil && rng.Start.Byte <= diag.Range.Start.Byte && diag.Range.Start.Byte < rng.End.Byte) {
			target, parent = wrap, p
		}
	}}
	hclsyntax.Walk(body, w)
	if target == nil {
		return nil
	}

	inner := target.Wrapped.Range()
	text := string(doc.Text[inner.Start.Byte:inner.End.Byte])
	if needsParens(target.Wrapped, parent) {
		text = "(" + text + ")"
	}
	title := fmt.Sprintf("Replace %s with %s", quoted(string(doc.Text[target.SrcRange.Start.Byte:target.SrcRange.End.Byte]), 40), quoted(text, 40))
	return []Action{{
		Title:     title,
		Preferred: true,
		Edits:     []Edit{replace(doc.Path, doc.Text, target.SrcRange.Start.Byte, target.SrcRange.End.Byte, text)},
	}}
}

// isPrimary tells whether an expression binds tighter than any operator,
// so that it can stand as an operand or be indexed as it is.
func isPrimary(expr hclsyntax.Expression) bool {
	switch expr.(type) {
	case *hclsyntax.ScopeTraversalExpr, *hclsyntax.RelativeTraversalExpr,
		*hclsyntax.FunctionCallExpr, *hclsyntax.LiteralValueExpr,
		*hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr,
		*hclsyntax.TupleConsExpr, *hclsyntax.ObjectConsExpr,
		*hclsyntax.IndexExpr, *hclsyntax.SplatExpr,
		*hclsyntax.ParenthesesExpr, *hclsyntax.ForExpr:
		return true
	}
	return false
}

// needsParens tells whether expr must be parenthesized to replace a
// primary expression whose parent is parent.
func needsParens(expr hclsyntax.Expression, parent hclsyntax.Node) bool {
	if _, ok := parent.(*hclsyntax.ObjectConsKeyExpr); ok {
		return true
	}
	if isPrimary(expr) {
		return false
	}
	switch parent.(type) {
	case *hclsyntax.BinaryOpExpr, *hclsyntax.UnaryOpExpr, *hclsyntax.ConditionalExpr,
		*hclsyntax.IndexExpr, *hclsyntax.SplatExpr, *hclsyntax.RelativeTraversalExpr:
		return true
	}
	return false
}
