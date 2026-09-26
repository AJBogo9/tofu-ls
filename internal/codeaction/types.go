// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// constraintType returns the type a schema constraint accepts, when it
// is one type.
func constraintType(c schema.Constraint) (cty.Type, bool) {
	switch c := c.(type) {
	case schema.LiteralType:
		return c.Type, c.Type != cty.NilType
	case schema.AnyExpression:
		return c.OfType, c.OfType != cty.NilType
	case schema.List:
		if elem, ok := constraintType(c.Elem); ok {
			return cty.List(elem), true
		}
	case schema.Set:
		if elem, ok := constraintType(c.Elem); ok {
			return cty.Set(elem), true
		}
	case schema.Map:
		if elem, ok := constraintType(c.Elem); ok {
			return cty.Map(elem), true
		}
	case schema.OneOf:
		// provider arguments list their whole type first
		for _, member := range c {
			if ty, ok := constraintType(member); ok {
				return ty, true
			}
		}
	}
	return cty.NilType, false
}

// isReference tells whether a constraint accepts only a reference (an
// address such as moved.from), which no placeholder value can fill.
func isReference(c schema.Constraint) bool {
	switch c := c.(type) {
	case schema.Reference:
		return true
	case schema.OneOf:
		for _, member := range c {
			if !isReference(member) {
				return false
			}
		}
		return len(c) > 0
	}
	return false
}

// declaredTypeString is the type constraint to declare for a variable
// holding values of type ty, or "" when no type can be written without
// guessing: an unknown type, or any.
func declaredTypeString(ty cty.Type) string {
	if ty == cty.NilType || ty == cty.DynamicPseudoType {
		return ""
	}
	return typeexpr.TypeString(ty)
}

// generalized turns the type of a literal value into the type a variable
// holding it is usually declared with: a tuple of one element type is a
// list, and an object whose attributes share one type is a map. Empty
// collections and nulls have no type to infer.
func generalized(ty cty.Type) (cty.Type, bool) {
	switch {
	case ty == cty.NilType || ty == cty.DynamicPseudoType:
		return cty.NilType, false
	case ty.IsPrimitiveType():
		return ty, true
	case ty.IsTupleType():
		elems := ty.TupleElementTypes()
		if len(elems) == 0 {
			return cty.NilType, false
		}
		if elem, ok := common(elems); ok {
			return cty.List(elem), true
		}
		return cty.NilType, false
	case ty.IsObjectType():
		attrs := ty.AttributeTypes()
		if len(attrs) == 0 {
			return cty.NilType, false
		}
		list := make([]cty.Type, 0, len(attrs))
		for _, at := range attrs {
			list = append(list, at)
		}
		if elem, ok := common(list); ok {
			return cty.Map(elem), true
		}
		obj := make(map[string]cty.Type, len(attrs))
		for name, at := range attrs {
			g, ok := generalized(at)
			if !ok {
				return cty.NilType, false
			}
			obj[name] = g
		}
		return cty.Object(obj), true
	case ty.IsListType(), ty.IsSetType(), ty.IsMapType():
		elem, ok := generalized(ty.ElementType())
		if !ok {
			return cty.NilType, false
		}
		switch {
		case ty.IsListType():
			return cty.List(elem), true
		case ty.IsSetType():
			return cty.Set(elem), true
		}
		return cty.Map(elem), true
	}
	return cty.NilType, false
}

// common is the one generalized type of all of types.
func common(types []cty.Type) (cty.Type, bool) {
	var first cty.Type
	for i, ty := range types {
		g, ok := generalized(ty)
		if !ok {
			return cty.NilType, false
		}
		if i == 0 {
			first = g
			continue
		}
		if !g.Equals(first) {
			return cty.NilType, false
		}
	}
	return first, true
}

// placeholder is the value inserted for a required argument of a type:
// an empty value of that type, or null when the type is not known. A
// number is 1 rather than 0, since providers commonly require at least 1
// (a length, a count, a size, a port) and tofu validate rejects 0 there.
func placeholder(c schema.Constraint) string {
	ty, ok := constraintType(c)
	if !ok {
		return "null"
	}
	switch {
	case ty == cty.String:
		return `""`
	case ty == cty.Number:
		return "1"
	case ty == cty.Bool:
		return "false"
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType():
		return "[]"
	case ty.IsMapType(), ty.IsObjectType():
		return "{}"
	}
	return "null"
}

// bodySchemaFor returns the schema of the body of the last block of
// chain, a path of nested blocks from the top of a file of the module in
// dir, or nil when it is not known.
func bodySchemaFor(env Env, dir string, chain []*hclsyntax.Block) *schema.BodySchema {
	if env.Schema == nil {
		return nil
	}
	bs := env.Schema(dir)
	for _, block := range chain {
		if bs == nil {
			return nil
		}
		blockSchema, ok := bs.Blocks[block.Type]
		if !ok || blockSchema == nil {
			return nil
		}
		bs = decoder.MergedBlockBodySchema(block, blockSchema)
	}
	return bs
}

// argumentType returns the type the schema expects for the attribute
// name of the body of the last block of chain.
func argumentType(env Env, dir string, chain []*hclsyntax.Block, name string) (cty.Type, bool) {
	if len(chain) == 0 {
		return cty.NilType, false
	}
	bs := bodySchemaFor(env, dir, chain)
	if bs == nil {
		return cty.NilType, false
	}
	if name == "count" && bs.Extensions != nil && bs.Extensions.Count {
		return cty.Number, true
	}
	if a, ok := bs.Attributes[name]; ok && a != nil {
		return constraintType(a.Constraint)
	}
	return cty.NilType, false
}

// blockChain returns the blocks from the top of body down to the block
// whose body is at rng (or, failing an exact match, the innermost block
// whose body holds the start of rng).
func blockChain(body *hclsyntax.Body, rng hcl.Range) []*hclsyntax.Block {
	var best []*hclsyntax.Block
	var walk func(body *hclsyntax.Body, chain []*hclsyntax.Block) bool
	walk = func(body *hclsyntax.Body, chain []*hclsyntax.Block) bool {
		for _, block := range body.Blocks {
			br := block.Body.SrcRange
			if br.Start.Byte > rng.Start.Byte || br.End.Byte < rng.Start.Byte {
				continue
			}
			next := append(append([]*hclsyntax.Block{}, chain...), block)
			if br.Start.Byte == rng.Start.Byte && br.End.Byte == rng.End.Byte {
				best = next
				return true
			}
			best = next
			if walk(block.Body, next) {
				return true
			}
		}
		return false
	}
	walk(body, nil)
	return best
}
