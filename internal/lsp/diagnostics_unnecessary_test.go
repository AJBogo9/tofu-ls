// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func TestHCLDiagsToLSP_unnecessaryCode(t *testing.T) {
	testCases := []struct {
		name             string
		diag             *hcl.Diagnostic
		expectedSeverity lsp.DiagnosticSeverity
		expectedTags     []lsp.DiagnosticTag
	}{
		{
			"plain warning",
			&hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "w"},
			lsp.SeverityWarning,
			nil,
		},
		{
			"unused symbol",
			&hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "unused", Extra: UnnecessaryCode{}},
			lsp.SeverityHint,
			[]lsp.DiagnosticTag{lsp.Unnecessary},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			diags := HCLDiagsToLSP(hcl.Diagnostics{tc.diag}, "OpenTofu")
			if len(diags) != 1 {
				t.Fatalf("expected 1 diagnostic, got %d", len(diags))
			}
			if diags[0].Severity != tc.expectedSeverity {
				t.Fatalf("expected severity %v, got %v", tc.expectedSeverity, diags[0].Severity)
			}
			if fmt.Sprint(diags[0].Tags) != fmt.Sprint(tc.expectedTags) {
				t.Fatalf("expected tags %v, got %v", tc.expectedTags, diags[0].Tags)
			}
		})
	}
}
