// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func HCLSeverityToLSP(severity hcl.DiagnosticSeverity) lsp.DiagnosticSeverity {
	var sev lsp.DiagnosticSeverity
	switch severity {
	case hcl.DiagError:
		sev = lsp.SeverityError
	case hcl.DiagWarning:
		sev = lsp.SeverityWarning
	case hcl.DiagInvalid:
		panic("invalid diagnostic")
	}
	return sev
}

func HCLDiagsToLSP(hclDiags hcl.Diagnostics, source string) []lsp.Diagnostic {
	diags := []lsp.Diagnostic{}

	for _, hclDiag := range hclDiags {
		msg := hclDiag.Summary
		if hclDiag.Detail != "" {
			msg += ": " + hclDiag.Detail
		}
		var rnge lsp.Range
		if hclDiag.Subject != nil {
			rnge = HCLRangeToLSP(*hclDiag.Subject)
		}
		diag := lsp.Diagnostic{
			Range:    rnge,
			Severity: HCLSeverityToLSP(hclDiag.Severity),
			Source:   source,
			Message:  msg,
		}
		if _, ok := hclDiag.Extra.(UnnecessaryCode); ok {
			// a hint the client renders faded, not a problem
			diag.Severity = lsp.SeverityHint
			diag.Tags = []lsp.DiagnosticTag{lsp.Unnecessary}
		}
		diags = append(diags, diag)

	}
	return diags
}

// UnnecessaryCode marks (as hcl.Diagnostic.Extra) a diagnostic for code
// which has no effect, such as an unused variable. It is published as a
// hint with the Unnecessary tag, which clients render faded.
type UnnecessaryCode struct{}
