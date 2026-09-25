// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package selection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

func TestRanges(t *testing.T) {
	src := `resource "x" "y" {
  name = "app-${var.stage}"
  tags = {
    team = local.team
  }
}
`
	lines := strings.Split(src, "\n")
	text := func(rng hcl.Range) string {
		return src[rng.Start.Byte:rng.End.Byte]
	}
	offset := func(line, col int) int {
		o := 0
		for i := 0; i < line-1; i++ {
			o += len(lines[i]) + 1
		}
		return o + col - 1
	}

	testCases := []struct {
		name     string
		line     int
		col      int
		expected []string
	}{
		{
			"inside a reference in a template",
			2, 22,
			[]string{"stage", "var.stage", `"app-${var.stage}"`, `name = "app-${var.stage}"`},
		},
		{
			"name label",
			1, 16,
			[]string{`"y"`},
		},
		{
			"object value",
			4, 14,
			[]string{"local", "local.team", "{\n    team = local.team\n  }"},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			pos := hcl.Pos{Line: tc.line, Column: tc.col, Byte: offset(tc.line, tc.col)}
			got := make([]string, 0)
			for _, rng := range Ranges([]byte(src), "main.tf", pos) {
				got = append(got, text(rng))
			}
			if len(got) < len(tc.expected) {
				t.Fatalf("expected at least %d ranges, got %q", len(tc.expected), got)
			}
			for j, want := range tc.expected {
				if got[j] != want {
					t.Fatalf("range %d: expected %q, got %q (all: %q)", j, want, got[j], got)
				}
			}
			// the chain always ends with the whole file
			if got[len(got)-1] != src && got[len(got)-1] != strings.TrimSuffix(src, "\n") {
				t.Fatalf("expected the last range to be the file, got %q", got[len(got)-1])
			}
		})
	}
}
