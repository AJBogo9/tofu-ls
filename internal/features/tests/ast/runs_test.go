// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package ast

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestRuns(t *testing.T) {
	root := filepath.FromSlash("/work/mod")

	testCases := []struct {
		name string
		src  string
		want map[string]string
	}{
		{
			"root module",
			`run "first" {}`,
			map[string]string{"first": root},
		},
		{
			"local module",
			`run "child" {
  module {
    source = "./modules/child"
  }
}`,
			map[string]string{"child": filepath.Join(root, "modules", "child")},
		},
		{
			"parent directory",
			`run "up" {
  module { source = "../shared" }
}`,
			map[string]string{"up": filepath.FromSlash("/work/shared")},
		},
		{
			"registry module",
			`run "reg" {
  module {
    source  = "terraform-aws-modules/vpc/aws"
    version = "~> 5.0"
  }
}`,
			map[string]string{"reg": ""},
		},
		{
			"module block without source",
			`run "broken" {
  module {}
}`,
			map[string]string{"broken": ""},
		},
		{
			"other blocks",
			`variables {
  x = 1
}
provider "null" {}
run "a" {}
run "b" {}`,
			map[string]string{"a": root, "b": root},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := hclsyntax.ParseConfig([]byte(tc.src), "a.tftest.hcl", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			got := map[string]string{}
			for _, run := range Runs(f.Body.(*hclsyntax.Body), root) {
				got[run.Name] = run.Module
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("modules: %s", diff)
			}
		})
	}
}

func TestModuleRoot(t *testing.T) {
	modules := map[string]bool{
		filepath.FromSlash("/work/mod"):         true,
		filepath.FromSlash("/work/mod/example"): true,
	}
	isModule := func(dir string) bool { return modules[dir] }

	testCases := []struct {
		dir  string
		want string
	}{
		{"/work/mod", "/work/mod"},
		{"/work/mod/tests", "/work/mod"},
		{"/work/mod/example", "/work/mod/example"},
		{"/work/other/tests", "/work/other/tests"},
	}
	for _, tc := range testCases {
		t.Run(tc.dir, func(t *testing.T) {
			if got := ModuleRoot(filepath.FromSlash(tc.dir), isModule); got != filepath.FromSlash(tc.want) {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}
