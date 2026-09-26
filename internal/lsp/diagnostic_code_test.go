// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func TestHCLDiagsToLSP_diagnosticCode(t *testing.T) {
	diags := HCLDiagsToLSP(hcl.Diagnostics{
		&hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "File not found",
			Extra: DiagnosticCode{
				Code: "template-file-missing",
				Data: map[string]any{"path": "/m/templates/a.tftpl"},
			},
		},
		&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "plain"},
	}, "OpenTofu")

	expected := []lsp.Diagnostic{
		{
			Severity: lsp.SeverityWarning,
			Source:   "OpenTofu",
			Message:  "File not found",
			Code:     "template-file-missing",
			Data:     map[string]any{"path": "/m/templates/a.tftpl"},
		},
		{
			Severity: lsp.SeverityError,
			Source:   "OpenTofu",
			Message:  "plain",
		},
	}
	if diff := cmp.Diff(expected, diags); diff != "" {
		t.Fatalf("unexpected diagnostics: %s", diff)
	}
}
