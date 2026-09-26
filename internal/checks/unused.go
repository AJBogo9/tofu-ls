// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// unusedDataSources reports data sources which nothing in the module
// references, as hints which clients render faded. Resources are never
// reported: they exist for their side effects.
//
// Left out: data sources in check blocks and those with a precondition
// or postcondition (both exist to be checked), and data sources which
// other blocks list in depends_on (a reference like any other). Nothing
// is reported while a file is broken or when the module has JSON files,
// whose references are not searched.
func (c *checker) unusedDataSources() {
	if !c.complete {
		return
	}
	for _, name := range c.names {
		if isJSON(name) {
			return
		}
	}

	used := make(map[string]bool)
	for _, name := range c.names {
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			return
		}
		walkExprs(body, func(expr hclsyntax.Expression) {
			hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
				if st, ok := node.(*hclsyntax.ScopeTraversalExpr); ok && st.Traversal.RootName() == "data" {
					if typ, n, ok := attrSteps(st.Traversal, 1); ok {
						used[typ+"."+n] = true
					}
				}
				return nil
			})
		})
	}

	checked := make(map[string]bool)
	for _, d := range c.idx.decls {
		if d.kind != "data" || d.checkScoped {
			continue
		}
		if hasConditions(d.block.Body) {
			checked[d.address] = true
		}
	}

	for _, d := range c.idx.decls {
		if d.kind != "data" || d.checkScoped || d.override || d.shadowed {
			continue
		}
		if used[d.typ+"."+d.name] || checked[d.address] {
			continue
		}
		c.add(hcl.DiagWarning, fmt.Sprintf("Data source %q is declared but not used", d.address), "",
			d.rng, ilsp.CodedDiagnostic{
				Code:        ilsp.CodeUnusedDataSource,
				Data:        map[string]interface{}{"address": d.address},
				Unnecessary: true,
			})
	}
}

// hasConditions reports whether a block's lifecycle has a precondition
// or a postcondition.
func hasConditions(body hcl.Body) bool {
	syn, ok := body.(*hclsyntax.Body)
	if !ok {
		return true
	}
	for _, block := range syn.Blocks {
		if block.Type != "lifecycle" {
			continue
		}
		for _, inner := range block.Body.Blocks {
			if inner.Type == "precondition" || inner.Type == "postcondition" {
				return true
			}
		}
	}
	return false
}

// walkExprs calls fn for every attribute expression of a body, nested
// blocks included.
func walkExprs(body *hclsyntax.Body, fn func(hclsyntax.Expression)) {
	for _, attr := range body.Attributes {
		fn(attr.Expr)
	}
	for _, block := range body.Blocks {
		walkExprs(block.Body, fn)
	}
}
