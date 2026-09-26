// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// interpolationOnly hints at templates which only wrap one expression,
// such as "${var.x}". HCL returns the wrapped value unchanged (it is not
// even converted to a string), so the quotes and ${ } can go.
func (c *checker) interpolationOnly() {
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		walkExprs(body, func(expr hclsyntax.Expression) {
			hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
				wrap, ok := node.(*hclsyntax.TemplateWrapExpr)
				if !ok {
					return nil
				}
				c.add(hcl.DiagWarning, "Interpolation-only expression",
					"This template only wraps an expression; it can be written without the quotes and ${ }.",
					wrap.SrcRange, ilsp.CodedDiagnostic{Code: ilsp.CodeInterpolationOnly, Hint: true})
				return nil
			})
		})
	}
}
