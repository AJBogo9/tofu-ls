// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/zclconf/go-cty/cty"
)

func TestAttributeInfo(t *testing.T) {
	petBody := &schema.BodySchema{
		Attributes: map[string]*schema.AttributeSchema{
			"id": {
				Description: lang.Markdown("The random pet name."),
				IsComputed:  true,
				Constraint:  schema.LiteralType{Type: cty.String},
			},
			"length": {
				IsOptional: true,
				Constraint: schema.LiteralType{Type: cty.Number},
			},
		},
		DocsLink: &schema.DocsLink{URL: "https://search.opentofu.org/provider/hashicorp/random/v3.6.3/docs/resources/pet"},
	}
	aliasBody := &schema.BodySchema{
		Attributes: map[string]*schema.AttributeSchema{
			"only_in_alias": {IsRequired: true, IsSensitive: true},
		},
	}
	bodySchema := &schema.BodySchema{
		Blocks: map[string]*schema.BlockSchema{
			"resource": {
				DependentBody: map[schema.SchemaKey]*schema.BodySchema{
					schema.NewSchemaKey(schema.DependencyKeys{
						Labels: []schema.LabelDependent{{Index: 0, Value: "random_pet"}},
					}): petBody,
					schema.NewSchemaKey(schema.DependencyKeys{
						Labels: []schema.LabelDependent{{Index: 0, Value: "other_thing"}},
						Attributes: []schema.AttributeDependent{{
							Name: "provider",
							Expr: schema.ExpressionValue{Address: lang.Address{lang.RootStep{Name: "other"}, lang.AttrStep{Name: "alias"}}},
						}},
					}): aliasBody,
				},
			},
		},
	}

	testCases := []struct {
		name      string
		blockType string
		typeName  string
		attr      string
		expected  *staticval.AttributeInfo
	}{
		{
			"computed attribute",
			"resource", "random_pet", "id",
			&staticval.AttributeInfo{
				Type:        "string",
				Description: "The random pet name.",
				Computed:    true,
				DocsURL:     "https://search.opentofu.org/provider/hashicorp/random/v3.6.3/docs/resources/pet",
			},
		},
		{
			"optional attribute",
			"resource", "random_pet", "length",
			&staticval.AttributeInfo{
				Type:     "number",
				Optional: true,
				DocsURL:  "https://search.opentofu.org/provider/hashicorp/random/v3.6.3/docs/resources/pet",
			},
		},
		{
			"body that depends on a provider alias",
			"resource", "other_thing", "only_in_alias",
			&staticval.AttributeInfo{Required: true, Sensitive: true},
		},
		{"unknown attribute", "resource", "random_pet", "nope", nil},
		{"unknown type", "resource", "random_id", "id", nil},
		{"unknown block type", "data", "random_pet", "id", nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := attributeInfo(bodySchema, tc.blockType, tc.typeName, tc.attr)
			if ok != (tc.expected != nil) {
				t.Fatalf("expected found=%t, got %t", tc.expected != nil, ok)
			}
			if diff := cmp.Diff(tc.expected, info); diff != "" {
				t.Fatalf("unexpected attribute info: %s", diff)
			}
		})
	}
	if _, ok := attributeInfo(nil, "resource", "random_pet", "id"); ok {
		t.Fatal("expected no attribute info without a schema")
	}
}
