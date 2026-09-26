// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package validations

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl-lang/schemacontext"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

type MissingRequiredAttribute struct{}

func (mra MissingRequiredAttribute) Visit(ctx context.Context, node hclsyntax.Node, nodeSchema schema.Schema) (context.Context, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if HasUnknownRequiredAttributes(ctx) {
		return ctx, diags
	}

	switch nodeType := node.(type) {
	case *hclsyntax.Block:
		// Providers are excluded from the validation for the time being
		// due to complexity around required attributes with dynamic defaults
		// See https://github.com/hashicorp/vscode-terraform/issues/1616
		nestingLvl, nestingOk := schemacontext.BlockNestingLevel(ctx)
		if nodeType.Type == "provider" && (nestingOk && nestingLvl == 0) {
			ctx = WithUnknownRequiredAttributes(ctx)
		}
	case *hclsyntax.Body:
		if nodeSchema == nil {
			return ctx, diags
		}

		bodySchema := nodeSchema.(*schema.BodySchema)
		if bodySchema.Attributes == nil {
			return ctx, diags
		}

		missingAttrs, missingBlocks := missingRequired(nodeType, bodySchema)
		for name, attr := range bodySchema.Attributes {
			if attr.IsRequired {
				_, ok := nodeType.Attributes[name]
				if !ok {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("Required attribute %q not specified", name),
						Detail:   fmt.Sprintf("An attribute named %q is required here", name),
						Subject:  nodeType.SrcRange.Ptr(),
						Extra: ilsp.CodedDiagnostic{
							Code: ilsp.CodeMissingRequiredAttribute,
							Data: map[string]interface{}{
								"attributes": missingAttrs,
								"blocks":     missingBlocks,
							},
						},
					})
				}
			}
		}
	}

	return ctx, diags
}

// missingRequired returns the names of the body's missing required
// attributes, and of the block types it has fewer of than required, in
// order, so that one fix can add them all.
func missingRequired(body *hclsyntax.Body, bodySchema *schema.BodySchema) ([]string, []string) {
	attrs := make([]string, 0)
	for name, attr := range bodySchema.Attributes {
		if _, ok := body.Attributes[name]; attr.IsRequired && !ok {
			attrs = append(attrs, name)
		}
	}
	sort.Strings(attrs)

	blocks := make([]string, 0)
	for name, block := range bodySchema.Blocks {
		if block.MinItems == 0 {
			continue
		}
		count := 0
		for _, b := range body.Blocks {
			if b.Type == name {
				count++
			}
		}
		if uint64(count) < block.MinItems {
			blocks = append(blocks, name)
		}
	}
	sort.Strings(blocks)

	return attrs, blocks
}

type unknownRequiredAttrsCtxKey struct{}

func HasUnknownRequiredAttributes(ctx context.Context) bool {
	_, ok := ctx.Value(unknownRequiredAttrsCtxKey{}).(bool)
	return ok
}

func WithUnknownRequiredAttributes(ctx context.Context) context.Context {
	return context.WithValue(ctx, unknownRequiredAttrsCtxKey{}, true)
}
