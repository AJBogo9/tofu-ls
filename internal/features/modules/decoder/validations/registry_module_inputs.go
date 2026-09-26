// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package validations

import (
	"context"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl-lang/schemacontext"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// RegistryModuleInputs leaves the inputs of the module calls named in
// Calls unchecked for being unexpected: their schema comes from the
// registry's data about the module, not from the module itself. That
// data can be incomplete (the registry lists no variables at all for
// some module versions) and arrives while the module is validated, so an
// input it does not list is not known to be wrong. It must run before
// validator.UnexpectedAttribute and validator.UnexpectedBlock.
type RegistryModuleInputs struct {
	Calls map[string]bool
}

func (v RegistryModuleInputs) Visit(ctx context.Context, node hclsyntax.Node, nodeSchema schema.Schema) (context.Context, hcl.Diagnostics) {
	block, ok := node.(*hclsyntax.Block)
	if !ok || block.Type != "module" || len(block.Labels) != 1 || !v.Calls[block.Labels[0]] {
		return ctx, nil
	}
	if lvl, ok := schemacontext.BlockNestingLevel(ctx); !ok || lvl != 0 {
		return ctx, nil
	}
	// the body of the call is walked with this context
	return schemacontext.WithUnknownSchema(ctx), nil
}
