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
		if coded, ok := hclDiag.Extra.(CodedDiagnostic); ok {
			diag.Code = coded.Code
			if len(coded.Data) > 0 {
				diag.Data = coded.Data
			}
			if coded.Hint || coded.Unnecessary {
				diag.Severity = lsp.SeverityHint
			}
			if coded.Unnecessary {
				diag.Tags = []lsp.DiagnosticTag{lsp.Unnecessary}
			}
			for _, rel := range coded.Related {
				diag.RelatedInformation = append(diag.RelatedInformation, lsp.DiagnosticRelatedInformation{
					Location: lsp.Location{
						URI:   lsp.DocumentURI(rel.URI),
						Range: HCLRangeToLSP(rel.Range),
					},
					Message: rel.Message,
				})
			}
		}
		diags = append(diags, diag)

	}
	return diags
}

// UnnecessaryCode marks (as hcl.Diagnostic.Extra) a diagnostic for code
// which has no effect, such as an unused variable. It is published as a
// hint with the Unnecessary tag, which clients render faded.
type UnnecessaryCode struct{}

// CodedDiagnostic (as hcl.Diagnostic.Extra) gives a diagnostic a stable
// code and the data a code action needs to fix it. Both are published
// with the diagnostic and come back in textDocument/codeAction.
//
// The codes, their severities and data keys are the contract with the
// code actions (see the Code* constants).
type CodedDiagnostic struct {
	Code string
	// Data is published as the diagnostic's data; it must marshal to JSON.
	Data map[string]interface{}
	// Hint publishes the diagnostic as a hint, whatever its severity.
	Hint bool
	// Unnecessary publishes the diagnostic as a hint with the Unnecessary
	// tag, which clients render faded.
	Unnecessary bool
	// Related locations, such as the first of two duplicate declarations.
	Related []RelatedLocation
}

// RelatedLocation is a location a CodedDiagnostic points to.
type RelatedLocation struct {
	URI     string
	Range   hcl.Range
	Message string
}

// Diagnostic codes. They are stable: code actions and users' settings
// match on them.
const (
	CodeDuplicateDeclaration     = "duplicate-declaration"
	CodeUnresolvedReference      = "unresolved-reference"
	CodeUnavailableScope         = "unavailable-scope"
	CodeUnknownResourceType      = "unknown-resource-type"
	CodeInvalidTypeConstraint    = "invalid-type-constraint"
	CodeDefaultTypeMismatch      = "default-type-mismatch"
	CodeTfvarsUndeclared         = "tfvars-undeclared-variable"
	CodeTfvarsTypeMismatch       = "tfvars-type-mismatch"
	CodeTfvarsNotStatic          = "tfvars-not-static"
	CodeStaticReferenceRequired  = "static-reference-required"
	CodeOperandTypeMismatch      = "operand-type-mismatch"
	CodeModuleNotInstalled       = "module-not-installed"
	CodeModuleSourceMissing      = "module-source-missing"
	CodeProviderNotInstalled     = "provider-not-installed"
	CodeMissingRequiredAttribute = "missing-required-attribute"
	CodeUnusedVariable           = "unused-variable"
	CodeUnusedLocal              = "unused-local"
	CodeUnusedDataSource         = "unused-data-source"
	CodeInterpolationOnly        = "interpolation-only"
)
