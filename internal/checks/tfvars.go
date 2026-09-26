// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// tfvars checks the .tfvars files of the module's directory the way
// OpenTofu reads them:
//
//   - a value must be static: no references, no function calls;
//   - a name the module does not declare gets a warning (OpenTofu warns
//     too, and errors only for -var);
//   - a value must convert to its variable's type, after the type's
//     optional attribute defaults are applied.
//
// The last two need every variable declaration, so they are skipped
// while a configuration file is broken.
func (c *checker) tfvars() {
	varTypes := c.tfvarsTypes()
	for _, name := range sortedKeys(c.mod.VarsFiles) {
		f := c.mod.VarsFiles[name]
		if f == nil || c.mod.BrokenVars[name] {
			continue
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
		for _, attr := range body.Attributes {
			attrs = append(attrs, attr)
		}
		sort.Slice(attrs, func(i, j int) bool { return attrs[i].SrcRange.Start.Byte < attrs[j].SrcRange.Start.Byte })

		for _, attr := range attrs {
			c.tfvarsAttr(name, attr, varTypes)
		}
	}
}

type tfvarsType struct {
	ty       cty.Type
	defaults *typeexpr.Defaults
}

// tfvarsTypes returns the declared variables, with their type where it
// is certain: declared once, and not in an override file (which could
// change it). A nil map means the declarations are not complete.
func (c *checker) tfvarsTypes() map[string]*tfvarsType {
	if !c.complete || c.mod.Meta == nil {
		return nil
	}
	count := make(map[string]int)
	for _, d := range c.idx.decls {
		if d.kind == "variable" {
			count[d.name]++
		}
	}
	types := make(map[string]*tfvarsType, len(c.mod.Meta.Variables))
	for name, v := range c.mod.Meta.Variables {
		types[name] = nil
		if count[name] != 1 || c.idx.overridden["var."+name] {
			continue
		}
		if v.Type == cty.NilType || v.Type == cty.DynamicPseudoType {
			continue
		}
		types[name] = &tfvarsType{ty: v.Type, defaults: v.TypeDefaults}
	}
	// variables the early decoder skipped are still declared
	for name := range count {
		if _, ok := types[name]; !ok {
			types[name] = nil
		}
	}
	return types
}

func (c *checker) tfvarsAttr(file string, attr *hclsyntax.Attribute, varTypes map[string]*tfvarsType) {
	notStatic := false
	for _, tr := range attr.Expr.Variables() {
		notStatic = true
		c.add(hcl.DiagError, "Variables not allowed", "Variables may not be used here.",
			tr.SourceRange(), ilsp.CodedDiagnostic{Code: ilsp.CodeTfvarsNotStatic})
	}
	hclsyntax.VisitAll(attr.Expr, func(node hclsyntax.Node) hcl.Diagnostics {
		if call, ok := node.(*hclsyntax.FunctionCallExpr); ok {
			notStatic = true
			c.add(hcl.DiagError, "Function calls not allowed", "Functions may not be called here.",
				call.Range(), ilsp.CodedDiagnostic{Code: ilsp.CodeTfvarsNotStatic})
		}
		return nil
	})
	var val cty.Value
	static := false
	if !notStatic {
		var diags hcl.Diagnostics
		val, diags = attr.Expr.Value(nil)
		static = !diags.HasErrors()
	}

	if varTypes == nil {
		return
	}
	vt, declared := varTypes[attr.Name]
	if !declared {
		data := map[string]interface{}{"name": attr.Name}
		if static {
			data["valueType"] = typeConstraintFor(val.Type())
		}
		c.add(hcl.DiagWarning, "Value for undeclared variable",
			fmt.Sprintf("The module does not declare a variable named %q but a value was found in file %q. If you meant to use this value, add a \"variable\" block to the configuration.", attr.Name, file),
			attr.NameRange, ilsp.CodedDiagnostic{Code: ilsp.CodeTfvarsUndeclared, Data: data})
		return
	}
	if !static || vt == nil || val.IsNull() {
		return
	}
	if vt.defaults != nil {
		val = vt.defaults.Apply(val)
	}
	if _, err := convert.Convert(val, vt.ty); err != nil {
		c.add(hcl.DiagError, "Invalid value for input variable",
			fmt.Sprintf("The given value is not suitable for var.%s: %s.", attr.Name, err),
			attr.Expr.Range(), ilsp.CodedDiagnostic{
				Code: ilsp.CodeTfvarsTypeMismatch,
				Data: map[string]interface{}{"name": attr.Name, "type": typeexpr.TypeString(vt.ty)},
			})
	}
}

// typeConstraintFor returns a type constraint which accepts a value of
// the given type, written as a person would declare it: a tuple of one
// element type is a list, an object of one attribute type is a map.
func typeConstraintFor(ty cty.Type) string {
	switch {
	case ty == cty.DynamicPseudoType:
		return "any"
	case ty.IsTupleType():
		if elem, ok := commonType(ty.TupleElementTypes()); ok {
			return "list(" + typeConstraintFor(elem) + ")"
		}
	case ty.IsObjectType():
		attrs := ty.AttributeTypes()
		elems := make([]cty.Type, 0, len(attrs))
		for _, name := range sortedKeys(attrs) {
			elems = append(elems, attrs[name])
		}
		if elem, ok := commonType(elems); ok {
			return "map(" + typeConstraintFor(elem) + ")"
		}
	}
	return typeexpr.TypeString(ty)
}

// commonType returns the type every element has; any for none.
func commonType(types []cty.Type) (cty.Type, bool) {
	if len(types) == 0 {
		return cty.DynamicPseudoType, true
	}
	for _, t := range types[1:] {
		if !t.Equals(types[0]) {
			return cty.NilType, false
		}
	}
	if !types[0].IsPrimitiveType() {
		return cty.NilType, false
	}
	return types[0], true
}
