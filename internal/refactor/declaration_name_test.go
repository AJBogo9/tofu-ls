// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// The parsed file of the module's index gives the name ranges that
// reading and parsing the file again gives.
func TestDeclarationNameRangeInFile(t *testing.T) {
	src := []byte(`variable "region" {}

locals {
  prefix = "p"
}

resource "aws_vpc" "main" {}

data "aws_ami" "ubuntu" {}

module "net" {
  source = "./net"
}

output url {
  value = 1
}
`)
	dir := t.TempDir()
	path := lang.Path{Path: dir, LanguageID: "opentofu"}
	f, diags := hclsyntax.ParseConfig(src, "main.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	body := f.Body.(*hclsyntax.Body)
	blockRange := func(i int) *hcl.Range {
		rng := body.Blocks[i].Range()
		return &rng
	}
	addr := func(steps ...string) lang.Address {
		a := lang.Address{lang.RootStep{Name: steps[0]}}
		for _, s := range steps[1:] {
			a = append(a, lang.AttrStep{Name: s})
		}
		return a
	}
	prefixRng := body.Blocks[1].Body.Attributes["prefix"].SrcRange

	testCases := []struct {
		target   reference.Target
		expected string
	}{
		{reference.Target{Addr: addr("var", "region"), ScopeId: "variable", RangePtr: blockRange(0)}, "region"},
		{reference.Target{Addr: addr("local", "prefix"), ScopeId: "local", RangePtr: &prefixRng}, "prefix"},
		{reference.Target{Addr: addr("aws_vpc", "main"), ScopeId: "resource", RangePtr: blockRange(2)}, "main"},
		{reference.Target{Addr: addr("data", "aws_ami", "ubuntu"), ScopeId: "data", RangePtr: blockRange(3)}, "ubuntu"},
		{reference.Target{Addr: addr("module", "net"), ScopeId: "module", RangePtr: blockRange(4)}, "net"},
		{reference.Target{Addr: addr("output", "url"), ScopeId: "output", RangePtr: blockRange(5)}, "url"},
		// not a top-level declaration
		{reference.Target{Addr: addr("aws_vpc", "main", "id"), ScopeId: "resource", RangePtr: blockRange(2)}, ""},
	}

	env := Env{ReadFile: func(p string) ([]byte, error) {
		if p != filepath.Join(dir, "main.tf") {
			return nil, fmt.Errorf("unexpected read of %s", p)
		}
		return src, nil
	}}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.target.Addr.String()), func(t *testing.T) {
			rng, ok := DeclarationNameRangeInFile(path, tc.target, f)
			fromDisk, okFromDisk := DeclarationNameRange(env, path, tc.target)
			if ok != okFromDisk || rng != fromDisk {
				t.Fatalf("parsed file gives %v %v, reading the file gives %v %v", rng, ok, fromDisk, okFromDisk)
			}
			if tc.expected == "" {
				if ok {
					t.Fatalf("expected no name, got %v", rng)
				}
				return
			}
			if !ok || string(rng.SliceBytes(src)) != tc.expected {
				t.Fatalf("expected %q, got %q (%v)", tc.expected, rng.SliceBytes(src), ok)
			}
		})
	}
}
