// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// duplicates reports a second declaration of a variable, output, local
// value, module call, resource, data source or provider configuration.
//
// Override files merge into the primary declarations, so they may repeat
// them, and OpenTofu ignores a .tf file which a .tofu file shadows.
// Check-scoped data sources are left out.
func (c *checker) duplicates() {
	first := make(map[string]*decl)
	for _, d := range c.idx.decls {
		if d.override || d.shadowed || d.checkScoped || d.aliasUnknown || c.mod.Broken[d.file] {
			continue
		}
		prev, ok := first[d.address]
		if !ok {
			first[d.address] = d
			continue
		}

		summary, detail := duplicateMessage(d, prev)
		c.add(hcl.DiagError, summary, detail, d.rng, ilsp.CodedDiagnostic{
			Code: ilsp.CodeDuplicateDeclaration,
			Data: map[string]interface{}{
				"address": d.address,
				"first": map[string]interface{}{
					"uri":   c.fileURI(prev.file),
					"range": ilsp.HCLRangeToLSP(prev.rng),
				},
			},
			Related: []ilsp.RelatedLocation{{
				URI:     c.fileURI(prev.file),
				Range:   prev.rng,
				Message: "first declared here",
			}},
		})
	}
}

func duplicateMessage(d, prev *decl) (string, string) {
	at := prev.rng.String()
	switch d.kind {
	case "variable":
		return "Duplicate variable declaration",
			fmt.Sprintf("A variable named %q was already declared at %s. Variable names must be unique within a module.", d.name, at)
	case "output":
		return "Duplicate output definition",
			fmt.Sprintf("An output named %q was already defined at %s. Output names must be unique within a module.", d.name, at)
	case "local":
		return "Duplicate local value definition",
			fmt.Sprintf("A local value named %q was already defined at %s. Local value names must be unique within a module.", d.name, at)
	case "module":
		return "Duplicate module call",
			fmt.Sprintf("A module call named %q was already defined at %s. Module calls must have unique names within a module.", d.name, at)
	case "resource":
		return fmt.Sprintf("Duplicate resource %q configuration", d.typ),
			fmt.Sprintf("A %s resource named %q was already declared at %s. Resource names must be unique per type in each module.", d.typ, d.name, at)
	case "data":
		return fmt.Sprintf("Duplicate data %q configuration", d.typ),
			fmt.Sprintf("A %s data resource named %q was already declared at %s. Resource names must be unique per type in each module.", d.typ, d.name, at)
	case "ephemeral":
		return fmt.Sprintf("Duplicate ephemeral %q configuration", d.typ),
			fmt.Sprintf("A %s ephemeral resource named %q was already declared at %s. Resource names must be unique per type in each module.", d.typ, d.name, at)
	case "provider":
		if d.address == "provider."+d.name {
			return "Duplicate provider configuration",
				fmt.Sprintf("A default (non-aliased) provider configuration for %q was already given at %s. If multiple configurations are required, set the \"alias\" argument for alternative configurations.", d.name, at)
		}
		return "Duplicate provider configuration",
			fmt.Sprintf("A provider configuration for %q with alias %q was already given at %s. Each configuration for the same provider must have a distinct alias.", d.name, d.address[len("provider."+d.name+"."):], at)
	}
	return "Duplicate declaration", fmt.Sprintf("%s was already declared at %s.", d.address, at)
}
