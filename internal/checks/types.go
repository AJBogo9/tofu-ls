// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// variableTypes reports invalid type constraints, and defaults which do
// not convert to their variable's type, as OpenTofu does when it decodes
// a variable block: optional attribute defaults are applied first, and a
// null default is valid unless nullable is false. Override files may
// leave out the type, so their defaults are not checked.
func (c *checker) variableTypes() {
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type != "variable" || len(block.Labels) != 1 {
				continue
			}
			c.variableType(block, isOverrideFile(name))
		}
	}
}

func (c *checker) variableType(block *hclsyntax.Block, override bool) {
	typeAttr, hasType := block.Body.Attributes["type"]
	if !hasType {
		return
	}
	ty, defaults, ok := c.typeConstraint(typeAttr.Expr)
	if !ok || override {
		return
	}

	defAttr, hasDefault := block.Body.Attributes["default"]
	if !hasDefault {
		return
	}
	val, diags := defAttr.Expr.Value(nil)
	if diags.HasErrors() {
		// references and function calls are reported as static values
		return
	}
	name := block.Labels[0]
	data := map[string]interface{}{"variable": name, "type": typeexpr.TypeString(ty)}

	if val.IsNull() {
		if attr, ok := block.Body.Attributes["nullable"]; ok {
			if n, diags := attr.Expr.Value(nil); !diags.HasErrors() && n.Type() == cty.Bool && n.IsKnown() && !n.IsNull() && n.False() {
				c.add(hcl.DiagError, "Invalid default value for variable",
					"A null default value is not valid when nullable=false.",
					defAttr.Expr.Range(), ilsp.CodedDiagnostic{Code: ilsp.CodeDefaultTypeMismatch, Data: data})
			}
		}
		return
	}
	if defaults != nil {
		val = defaults.Apply(val)
	}
	if _, err := convert.Convert(val, ty); err != nil {
		c.add(hcl.DiagError, "Invalid default value for variable",
			fmt.Sprintf("This default value is not compatible with the variable's type constraint: %s.", err),
			defAttr.Expr.Range(), ilsp.CodedDiagnostic{Code: ilsp.CodeDefaultTypeMismatch, Data: data})
	}
}

// typeConstraint decodes a variable's type like OpenTofu does, reporting
// what it rejects: quoted (pre-0.12) types and invalid type expressions.
// The bare keywords list and map are still accepted, as list(any) and
// map(any).
func (c *checker) typeConstraint(expr hcl.Expression) (cty.Type, *typeexpr.Defaults, bool) {
	if tmpl, ok := expr.(*hclsyntax.TemplateExpr); ok && tmpl.IsStringLiteral() {
		val, _ := tmpl.Value(nil)
		str := val.AsString()
		detail := fmt.Sprintf("OpenTofu 0.11 and earlier required type constraints to be given in quotes, but that form is no longer supported. Remove the quotes around %q.", str)
		if str != "string" && str != "list" && str != "map" {
			detail = "The legacy variable type hint form, using a quoted string, allows only the values \"string\", \"list\", and \"map\". To provide a full type expression, remove the surrounding quotes and give the type expression directly."
		}
		c.add(hcl.DiagError, "Invalid quoted type constraints", detail, expr.Range(),
			ilsp.CodedDiagnostic{Code: ilsp.CodeInvalidTypeConstraint})
		return cty.NilType, nil, false
	}
	switch hcl.ExprAsKeyword(expr) {
	case "list":
		return cty.List(cty.DynamicPseudoType), nil, true
	case "map":
		return cty.Map(cty.DynamicPseudoType), nil, true
	}
	ty, defaults, diags := typeexpr.TypeConstraintWithDefaults(expr)
	if diags.HasErrors() {
		for _, d := range diags {
			if d.Severity != hcl.DiagError {
				continue
			}
			rng := expr.Range()
			if d.Subject != nil {
				rng = *d.Subject
			}
			c.add(hcl.DiagError, d.Summary, d.Detail, rng,
				ilsp.CodedDiagnostic{Code: ilsp.CodeInvalidTypeConstraint})
		}
		return cty.NilType, nil, false
	}
	return ty, defaults, true
}

// operands reports operands whose type is known and cannot convert to
// what the operator needs: arithmetic and comparison need numbers, and
// logical operators and conditions need bools. An operand's type is
// known when it evaluates without any reference or function call
// ("abc", true, [1]), or when it is a variable whose type constraint
// has no conversion at all (a bool or a list variable in arithmetic).
// A string such as "5" converts to a number, so it is fine.
func (c *checker) operands() {
	varTypes := c.knownVariableTypes()
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		c.operandsInBody(body, varTypes, "")
	}
}

func (c *checker) operandsInBody(body *hclsyntax.Body, varTypes map[string]cty.Type, blockType string) {
	for name, attr := range body.Attributes {
		if blockType == "variable" && name == "type" {
			continue
		}
		hclsyntax.VisitAll(attr.Expr, func(node hclsyntax.Node) hcl.Diagnostics {
			switch e := node.(type) {
			case *hclsyntax.BinaryOpExpr:
				want := e.Op.Impl.Params()[0].Type
				c.operand(e.LHS, want, "left operand", varTypes)
				c.operand(e.RHS, want, "right operand", varTypes)
			case *hclsyntax.UnaryOpExpr:
				c.operand(e.Val, e.Op.Impl.Params()[0].Type, "unary operand", varTypes)
			case *hclsyntax.ConditionalExpr:
				if mismatched(e.Condition, cty.Bool, varTypes) {
					c.add(hcl.DiagError, "Incorrect condition type",
						"The condition expression must be of type bool.",
						e.Condition.Range(), ilsp.CodedDiagnostic{Code: ilsp.CodeOperandTypeMismatch})
				}
			}
			return nil
		})
	}
	for _, block := range body.Blocks {
		c.operandsInBody(block.Body, varTypes, block.Type)
	}
}

func (c *checker) operand(expr hclsyntax.Expression, want cty.Type, which string, varTypes map[string]cty.Type) {
	if want == cty.DynamicPseudoType || !mismatched(expr, want, varTypes) {
		return
	}
	c.add(hcl.DiagError, "Invalid operand",
		fmt.Sprintf("Unsuitable value for %s: %s required.", which, friendlyTypeName(want)),
		expr.Range(), ilsp.CodedDiagnostic{Code: ilsp.CodeOperandTypeMismatch})
}

// mismatched reports whether an operand certainly cannot convert to the
// type an operator needs. A variable is judged by its type constraint;
// any other expression only when it evaluates without references or
// function calls, by its value ("5" converts to a number, "abc" does
// not). Null and unknown values are left alone.
func mismatched(expr hclsyntax.Expression, want cty.Type, varTypes map[string]cty.Type) bool {
	if st, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
		tr := st.Traversal
		if len(tr) != 2 || tr.RootName() != "var" {
			return false
		}
		name, ok := attrStep(tr, 1)
		if !ok {
			return false
		}
		ty, ok := varTypes[name]
		if !ok || ty.Equals(want) {
			return false
		}
		// a string variable may hold "5": only types with no conversion
		// at all are certain
		return convert.GetConversionUnsafe(ty, want) == nil
	}
	if len(expr.Variables()) > 0 || hasFunctionCall(expr) {
		return false
	}
	val, diags := expr.Value(nil)
	if diags.HasErrors() || val.IsNull() || !val.IsWhollyKnown() {
		return false
	}
	_, err := convert.Convert(val, want)
	return err != nil
}

func friendlyTypeName(ty cty.Type) string {
	switch ty {
	case cty.Number:
		return "a number is"
	case cty.Bool:
		return "a bool is"
	}
	return ty.FriendlyName() + " is"
}

func hasFunctionCall(expr hclsyntax.Expression) bool {
	found := false
	hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
		if _, ok := node.(*hclsyntax.FunctionCallExpr); ok {
			found = true
		}
		return nil
	})
	return found
}

// knownVariableTypes returns the types of the module's variables which
// are declared once, outside override files, with a valid type
// constraint.
func (c *checker) knownVariableTypes() map[string]cty.Type {
	types := make(map[string]cty.Type)
	seen := make(map[string]int)
	for _, d := range c.idx.decls {
		if d.kind == "variable" {
			seen[d.name]++
		}
	}
	for _, d := range c.idx.decls {
		if d.kind != "variable" || d.override || d.shadowed || seen[d.name] != 1 || c.idx.overridden[d.address] || isJSON(d.file) || c.mod.Broken[d.file] {
			continue
		}
		body, ok := d.block.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		attr, ok := body.Attributes["type"]
		if !ok {
			continue
		}
		if kw := hcl.ExprAsKeyword(attr.Expr); kw == "list" || kw == "map" {
			continue
		}
		ty, _, diags := typeexpr.TypeConstraintWithDefaults(attr.Expr)
		if diags.HasErrors() || ty.HasDynamicTypes() {
			continue
		}
		types[d.name] = ty
	}
	return types
}
