// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func TestHCLDiagsToLSP_coded(t *testing.T) {
	rng := hcl.Range{
		Filename: "main.tf",
		Start:    hcl.Pos{Line: 2, Column: 10, Byte: 20},
		End:      hcl.Pos{Line: 2, Column: 15, Byte: 25},
	}
	testCases := []struct {
		name     string
		diag     *hcl.Diagnostic
		expected string
	}{
		{
			"error with code and data",
			&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Duplicate variable declaration", Subject: &rng,
				Extra: CodedDiagnostic{
					Code: CodeDuplicateDeclaration,
					Data: map[string]interface{}{"address": "var.x"},
					Related: []RelatedLocation{{
						URI: "file:///m/main.tf", Range: rng, Message: "first declared here",
					}},
				}},
			`{"range":{"start":{"line":1,"character":9},"end":{"line":1,"character":14}},"severity":1,"code":"duplicate-declaration","source":"OpenTofu","message":"Duplicate variable declaration","relatedInformation":[{"location":{"uri":"file:///m/main.tf","range":{"start":{"line":1,"character":9},"end":{"line":1,"character":14}}},"message":"first declared here"}],"data":{"address":"var.x"}}`,
		},
		{
			"unnecessary hint",
			&hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "unused", Subject: &rng,
				Extra: CodedDiagnostic{Code: CodeUnusedLocal, Data: map[string]interface{}{"name": "x"}, Unnecessary: true}},
			`{"range":{"start":{"line":1,"character":9},"end":{"line":1,"character":14}},"severity":4,"code":"unused-local","source":"OpenTofu","message":"unused","tags":[1],"data":{"name":"x"}}`,
		},
		{
			"hint without data",
			&hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "wrap", Subject: &rng,
				Extra: CodedDiagnostic{Code: CodeInterpolationOnly, Hint: true}},
			`{"range":{"start":{"line":1,"character":9},"end":{"line":1,"character":14}},"severity":4,"code":"interpolation-only","source":"OpenTofu","message":"wrap"}`,
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			diags := HCLDiagsToLSP(hcl.Diagnostics{tc.diag}, "OpenTofu")
			if len(diags) != 1 {
				t.Fatalf("expected 1 diagnostic, got %d", len(diags))
			}
			b, err := json.Marshal(diags[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.expected {
				t.Fatalf("expected:\n%s\ngot:\n%s", tc.expected, b)
			}
			var roundTrip lsp.Diagnostic
			if err := json.Unmarshal(b, &roundTrip); err != nil {
				t.Fatal(err)
			}
		})
	}
}
