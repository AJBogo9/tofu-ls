// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func TestHCLRangeToLSPInText(t *testing.T) {
	testCases := []struct {
		name string
		src  string
		want lsp.Range
	}{
		{"ascii", "locals {\n  a = \"x ${var.stage}\"\n}\n", lsp.Range{Start: lsp.Position{Line: 1, Character: 15}, End: lsp.Position{Line: 1, Character: 20}}},
		// the emoji is one grapheme cluster, but two UTF-16 code units
		{"emoji", "locals {\n  a = \"\U0001F44B ${var.stage}\"\n}\n", lsp.Range{Start: lsp.Position{Line: 1, Character: 16}, End: lsp.Position{Line: 1, Character: 21}}},
		// e plus a combining acute accent is one grapheme cluster, but two
		// UTF-16 code units
		{"combining", "locals {\n  a = \"e\u0301 ${var.stage}\"\n}\n", lsp.Range{Start: lsp.Position{Line: 1, Character: 16}, End: lsp.Position{Line: 1, Character: 21}}},
		{"accented", "locals {\n  a = \"\u00e9 ${var.stage}\"\n}\n", lsp.Range{Start: lsp.Position{Line: 1, Character: 15}, End: lsp.Position{Line: 1, Character: 20}}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			f, diags := hclsyntax.ParseConfig(src, "main.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			var rng hcl.Range
			hclsyntax.VisitAll(f.Body.(*hclsyntax.Body), func(n hclsyntax.Node) hcl.Diagnostics {
				if tr, ok := n.(*hclsyntax.ScopeTraversalExpr); ok {
					// the name step, without its dot
					rng = tr.Traversal[1].SourceRange()
					rng.Start.Byte++
					rng.Start.Column++
				}
				return nil
			})
			if got := string(src[rng.Start.Byte:rng.End.Byte]); got != "stage" {
				t.Fatalf("test setup: range holds %q", got)
			}
			got := HCLRangeToLSPInText(rng, src)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected range: %s", diff)
			}
			// the range must cover exactly "stage" in UTF-16 terms
			line := strings.Split(tc.src, "\n")[got.Start.Line]
			units := utf16Units(line)
			if string(units[got.Start.Character:got.End.Character]) != "stage" {
				t.Fatalf("range covers %q", string(units[got.Start.Character:got.End.Character]))
			}
		})
	}
}

// utf16Units splits a line into UTF-16 code units, keeping ASCII readable.
func utf16Units(s string) []rune {
	var units []rune
	for _, r := range s {
		if r >= 0x10000 {
			units = append(units, '?', '?')
			continue
		}
		units = append(units, r)
	}
	return units
}
