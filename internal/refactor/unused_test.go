// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestUnusedSymbols(t *testing.T) {
	testCases := []struct {
		name     string
		files    map[string]string
		expected []string
	}{
		{
			"used and unused variables and locals",
			map[string]string{
				"variables.tf": `variable "used" {}
variable "unused" {}
variable "only_validated" {
  validation {
    condition     = length(var.only_validated) > 0
    error_message = "empty"
  }
}
`,
				"main.tf": `locals {
  a = var.used
  b = "${local.a}-x"
  c = 1
}
resource "terraform_data" "x" {
  input = local.b
}
`,
			},
			[]string{"variable only_validated variables.tf:3:10-3:26", "variable unused variables.tf:2:10-2:18", "local c main.tf:4:3-4:4"},
		},
		{
			"references inside schema-less blocks and conditions count",
			map[string]string{
				"main.tf": `variable "a" {}
variable "b" {}
check "c" {
  assert {
    condition     = var.a != ""
    error_message = "x"
  }
}
resource "unknown_thing" "x" {
  nested {
    value = [for s in var.b : s]
  }
}
`,
			},
			[]string{},
		},
		{
			"provider references are not local values",
			map[string]string{
				"main.tf": `locals {
  secondary = "x"
}
provider "local" {
  alias = "secondary"
}
resource "local_file" "f" {
  provider = local.secondary
}
`,
			},
			[]string{"local secondary main.tf:2:3-2:12"},
		},
		{
			"instance keys of provider references are values",
			map[string]string{
				"main.tf": `locals {
  pk = "a"
  mk = "b"
}
provider "random" {
  alias    = "by_key"
  for_each = toset(["a", "b"])
}
resource "random_id" "keyed" {
  provider    = random.by_key[local.pk]
  byte_length = 2
}
module "m" {
  source    = "./m"
  providers = { random = random.by_key[local.mk] }
}
`,
			},
			[]string{},
		},
		{
			"a JSON file means no report",
			map[string]string{
				"main.tf":       `variable "unused" {}`,
				"extra.tf.json": `{}`,
			},
			[]string{},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			files := map[string]*hcl.File{}
			for name, src := range tc.files {
				if strings.HasSuffix(name, ".json") {
					files[name] = &hcl.File{Bytes: []byte(src)}
					continue
				}
				f, diags := hclsyntax.ParseConfig([]byte(src), name, hcl.InitialPos)
				if diags.HasErrors() {
					t.Fatal(diags)
				}
				files[name] = f
			}
			got := make([]string, 0)
			for _, sym := range UnusedSymbols(files) {
				r := sym.NameRange
				got = append(got, fmt.Sprintf("%s %s %s:%d:%d-%d:%d", sym.Kind, sym.Name, sym.Filename, r.Start.Line, r.Start.Column, r.End.Line, r.End.Column))
			}
			if strings.Join(sorted(got), ",") != strings.Join(sorted(tc.expected), ",") {
				t.Fatalf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}
