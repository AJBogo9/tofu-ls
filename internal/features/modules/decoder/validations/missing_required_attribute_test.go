// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package validations

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

func TestMissingRequiredAttribute_code(t *testing.T) {
	bodySchema := &schema.BodySchema{
		Attributes: map[string]*schema.AttributeSchema{
			"length": {IsRequired: true},
			"name":   {IsRequired: true},
			"prefix": {IsOptional: true},
		},
		Blocks: map[string]*schema.BlockSchema{
			"rule":     {MinItems: 1, Body: &schema.BodySchema{}},
			"optional": {Body: &schema.BodySchema{}},
		},
	}
	testCases := []struct {
		name     string
		src      string
		expected []interface{}
	}{
		{
			"every missing name is in each diagnostic's data",
			`prefix = "x"`,
			[]interface{}{
				ilsp.CodedDiagnostic{
					Code: ilsp.CodeMissingRequiredAttribute,
					Data: map[string]interface{}{"attributes": []string{"length", "name"}, "blocks": []string{"rule"}},
				},
				ilsp.CodedDiagnostic{
					Code: ilsp.CodeMissingRequiredAttribute,
					Data: map[string]interface{}{"attributes": []string{"length", "name"}, "blocks": []string{"rule"}},
				},
			},
		},
		{
			"nothing missing",
			"length = 1\nname = \"x\"\nrule {}\n",
			[]interface{}{},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			f, diags := hclsyntax.ParseConfig([]byte(tc.src), "test.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			_, diags = MissingRequiredAttribute{}.Visit(context.Background(), f.Body.(*hclsyntax.Body), bodySchema)
			got := make([]interface{}, 0)
			for _, d := range diags {
				got = append(got, d.Extra)
			}
			if diff := cmp.Diff(tc.expected, got); diff != "" {
				t.Fatalf("unexpected extras: %s", diff)
			}
		})
	}
}
