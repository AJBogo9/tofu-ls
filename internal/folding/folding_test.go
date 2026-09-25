// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package folding

import (
	"fmt"
	"strings"
	"testing"
)

func TestRanges(t *testing.T) {
	testCases := []struct {
		name     string
		src      string
		expected []string
	}{
		{
			"empty file",
			``,
			[]string{},
		},
		{
			"single-line block does not fold",
			`variable "a" {}
`,
			[]string{},
		},
		{
			"block keeps its closing brace visible",
			`resource "x" "y" {
  a = 1
  b = 2
}
`,
			[]string{"0-2"},
		},
		{
			"nested blocks and a multi-line object",
			`resource "x" "y" {
  tags = {
    a = 1
  }
  lifecycle {
    ignore_changes = [
      tags,
    ]
  }
}
`,
			[]string{"0-8", "1-2", "4-7", "5-6"},
		},
		{
			"heredoc keeps its closing marker visible",
			`locals {
  banner = <<-EOT
    one
    two
  EOT
}
`,
			[]string{"0-4", "1-3"},
		},
		{
			"multi-line function call and for expression",
			`locals {
  merged = merge(
    var.a,
    var.b,
  )
  ports = [
    for s in var.services : s.port
  ]
}
`,
			[]string{"0-7", "1-3", "5-6"},
		},
		{
			"one range per start line",
			`locals {
  tags = merge({
    a = 1
  }, var.tags)
}
`,
			[]string{"0-3", "1-3"},
		},
		{
			"comment runs",
			`# one
# two
# three

// alone
variable "a" {} # trailing

/*
  block
*/
`,
			[]string{"0-2 comment", "7-9 comment"},
		},
		{
			"unparsable file still folds what it can",
			`resource "x" "y" {
  a = 1
  b =
}
# c1
# c2
`,
			[]string{"0-2", "4-5 comment"},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			got := make([]string, 0)
			for _, r := range Ranges([]byte(tc.src), "main.tf") {
				s := fmt.Sprintf("%d-%d", r.StartLine, r.EndLine)
				if r.Kind != "" {
					s += " " + r.Kind
				}
				got = append(got, s)
			}
			if strings.Join(got, ",") != strings.Join(tc.expected, ",") {
				t.Fatalf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}
