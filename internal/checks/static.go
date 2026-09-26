// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// staticValues reports expressions where OpenTofu allows only static
// values:
//
//   - a variable default may not reference anything or call a function;
//   - each depends_on entry must be a plain reference;
//   - module source and version, and backend arguments, are evaluated
//     before planning, so they may use input variables, local values and
//     path and terraform values, but not resources, data sources, module
//     outputs, each, count or self.
func (c *checker) staticValues() {
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			switch block.Type {
			case "variable":
				if attr, ok := block.Body.Attributes["default"]; ok {
					c.staticDefault(attr.Expr)
				}
			case "resource", "data", "ephemeral", "module", "output":
				if attr, ok := block.Body.Attributes["depends_on"]; ok {
					c.staticDependsOn(attr.Expr)
				}
				if block.Type == "module" {
					for _, arg := range []string{"source", "version"} {
						if attr, ok := block.Body.Attributes[arg]; ok {
							c.earlyEvaluated(attr.Expr, "module-source", fmt.Sprintf("The module %s", arg))
						}
					}
				}
			case "check":
				for _, inner := range block.Body.Blocks {
					if inner.Type != "data" {
						continue
					}
					if attr, ok := inner.Body.Attributes["depends_on"]; ok {
						c.staticDependsOn(attr.Expr)
					}
				}
			case "terraform":
				for _, inner := range block.Body.Blocks {
					if inner.Type == "backend" {
						c.earlyEvaluatedBody(inner.Body)
					}
				}
			}
		}
	}
}

func staticRequired(context string) ilsp.CodedDiagnostic {
	return ilsp.CodedDiagnostic{
		Code: ilsp.CodeStaticReferenceRequired,
		Data: map[string]interface{}{"context": context},
	}
}

func (c *checker) staticDefault(expr hclsyntax.Expression) {
	for _, tr := range expr.Variables() {
		c.add(hcl.DiagError, "Variables not allowed",
			"A variable default must be a static value: it cannot refer to variables, local values or other objects.",
			tr.SourceRange(), staticRequired("variable-default"))
	}
	hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
		if call, ok := node.(*hclsyntax.FunctionCallExpr); ok {
			c.add(hcl.DiagError, "Function calls not allowed",
				"A variable default must be a static value: functions may not be called here.",
				hcl.RangeBetween(call.NameRange, call.OpenParenRange), staticRequired("variable-default"))
		}
		return nil
	})
}

func (c *checker) staticDependsOn(expr hclsyntax.Expression) {
	list, ok := expr.(*hclsyntax.TupleConsExpr)
	if !ok {
		c.add(hcl.DiagError, "Invalid expression", "A static list expression is required.",
			expr.Range(), staticRequired("depends-on"))
		return
	}
	for _, item := range list.Exprs {
		if _, diags := hcl.AbsTraversalForExpr(item); diags.HasErrors() {
			c.add(hcl.DiagError, "Invalid expression",
				"A single static variable reference is required: only attribute access and indexing with constant keys. No calculations, function calls, template expressions, etc are allowed here.",
				item.Range(), staticRequired("depends-on"))
		}
	}
}

// earlyEvaluated reports references which OpenTofu cannot evaluate
// before planning.
func (c *checker) earlyEvaluated(expr hclsyntax.Expression, context, what string) {
	for _, tr := range expr.Variables() {
		switch tr.RootName() {
		case "var", "local", "path", "terraform":
			continue
		}
		c.add(hcl.DiagError, "Invalid reference in a static context",
			fmt.Sprintf("%s is evaluated before planning, so it can use only input variables, local values, path and terraform values. %s is not known then.", what, addressOf(tr, 3)),
			tr.SourceRange(), staticRequired(context))
	}
}

func (c *checker) earlyEvaluatedBody(body *hclsyntax.Body) {
	for _, attr := range body.Attributes {
		c.earlyEvaluated(attr.Expr, "backend", "A backend argument")
	}
	for _, block := range body.Blocks {
		c.earlyEvaluatedBody(block.Body)
	}
}
