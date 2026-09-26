// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package paths

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }

// writeTree writes files (by slash path) under a temporary directory and
// returns it. A name ending in "/" is a directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if name[len(name)-1] == '/' {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func parseExpr(t *testing.T, src string) hclsyntax.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "expr.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	return expr
}

func parseFile(t *testing.T, src string) *hcl.File {
	t.Helper()
	f, diags := hclsyntax.ParseConfig([]byte(src), "terragrunt.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	return f
}

func TestResolver_Path(t *testing.T) {
	root := writeTree(t, map[string]string{
		".git/":                           "",
		"root.hcl":                        "",
		"common/app.hcl":                  "",
		"live/prod/app/terragrunt.hcl":    "",
		"live/prod/region.hcl":            "",
		"live/prod/app/child/placeholder": "",
	})
	unitDir := filepath.Join(root, "live", "prod", "app")
	file := parseFile(t, `locals {
  common = "${get_repo_root()}/common"
  file   = "${local.common}/app.hcl"
  region = read_terragrunt_config("x.hcl")
  later  = "${local.file}.bak"
}`)

	testCases := []struct {
		name  string
		expr  string
		entry bool
		want  string
		ok    bool
	}{
		{"relative, in an entry file", `"../vpc"`, true, filepath.Join(root, "live", "prod", "vpc"), true},
		{"relative, in an included file", `"../vpc"`, false, "", false},
		{"absolute", `"/opt/x.hcl"`, false, "/opt/x.hcl", true},
		{"find_in_parent_folders walks up from the parent", `find_in_parent_folders("root.hcl")`, true, filepath.Join(root, "root.hcl"), true},
		{"find_in_parent_folders, the closest parent", `find_in_parent_folders("region.hcl")`, true, filepath.Join(root, "live", "prod", "region.hcl"), true},
		{"find_in_parent_folders skips the file's own directory", `find_in_parent_folders("terragrunt.hcl")`, true, "", false},
		{"find_in_parent_folders with a fallback", `find_in_parent_folders("nope.hcl", "/fallback.hcl")`, true, "/fallback.hcl", true},
		{"find_in_parent_folders without a name", `find_in_parent_folders()`, true, "", false},
		{"find_in_parent_folders depends on the includer", `find_in_parent_folders("root.hcl")`, false, "", false},
		{"dirname of find_in_parent_folders", `"${dirname(find_in_parent_folders("root.hcl"))}/common/app.hcl"`, true, filepath.Join(root, "common", "app.hcl"), true},
		{"get_terragrunt_dir", `"${get_terragrunt_dir()}/child"`, true, filepath.Join(unitDir, "child"), true},
		{"get_repo_root", `"${get_repo_root()}/common/app.hcl"`, true, filepath.Join(root, "common", "app.hcl"), true},
		{"static locals, in any order", `local.file`, true, filepath.Join(root, "common", "app.hcl"), true},
		{"a local from a later one", `local.later`, true, filepath.Join(root, "common", "app.hcl.bak"), true},
		{"a local that reads a file is not static", `local.region`, true, "", false},
		{"get_env is not static", `get_env("X", "/tmp")`, true, "", false},
		{"a function Terragrunt runs is not static", `"${path_relative_to_include()}/x"`, true, "", false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(osFS{}, unitDir, file, tc.entry)
			got, ok := r.Path(parseExpr(t, tc.expr))
			if ok != tc.ok || got != tc.want {
				t.Errorf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestResolver_targets(t *testing.T) {
	root := writeTree(t, map[string]string{
		"modules/vpc/main.tf":           "",
		"modules/vpc/variables.tf":      "",
		"modules/iam/b.tf":              "",
		"modules/iam/a.tofu":            "",
		"modules/empty/README.md":       "",
		"live/vpc/terragrunt.hcl":       "",
		"live/net/terragrunt.stack.hcl": "",
		"live/app/terragrunt.hcl":       "",
		"live/none/placeholder":         "",
	})
	r := NewResolver(osFS{}, filepath.Join(root, "live", "app"), nil, true)

	sources := []struct {
		source string
		want   string
		ok     bool
	}{
		{`"../../modules//vpc"`, filepath.Join(root, "modules", "vpc"), true},
		{`"../../modules/vpc"`, filepath.Join(root, "modules", "vpc"), true},
		{`"../../modules/vpc?ref=v1"`, filepath.Join(root, "modules", "vpc"), true},
		{`"git::https://example.com/modules.git//vpc?ref=v1"`, "", false},
		{`"tfr:///terraform-aws-modules/vpc/aws?version=5.0.0"`, "", false},
		{`"github.com/acme/modules//vpc"`, "", false},
		{`"../../modules/missing"`, "", false},
	}
	for _, tc := range sources {
		t.Run("source "+tc.source, func(t *testing.T) {
			got, ok := r.LocalSource(parseExpr(t, tc.source))
			if ok != tc.ok || got != tc.want {
				t.Errorf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}

	modules := []struct {
		dir  string
		want string
	}{
		{"vpc", "main.tf"},
		{"iam", "a.tofu"},
		{"empty", ""},
	}
	for _, tc := range modules {
		t.Run("module file of "+tc.dir, func(t *testing.T) {
			got, ok := r.ModuleFileIn(filepath.Join(root, "modules", tc.dir))
			want := ""
			if tc.want != "" {
				want = filepath.Join(root, "modules", tc.dir, tc.want)
			}
			if got != want || ok != (tc.want != "") {
				t.Errorf("got %q, %v; want %q", got, ok, want)
			}
		})
	}

	configs := []struct {
		path string
		want string
	}{
		{"live/vpc", "live/vpc/terragrunt.hcl"},
		{"live/net", "live/net/terragrunt.stack.hcl"},
		{"live/vpc/terragrunt.hcl", "live/vpc/terragrunt.hcl"},
		{"live/none", ""},
		{"live/missing", ""},
	}
	for _, tc := range configs {
		t.Run("config in "+tc.path, func(t *testing.T) {
			got, ok := r.ConfigIn(filepath.Join(root, filepath.FromSlash(tc.path)))
			want := ""
			if tc.want != "" {
				want = filepath.Join(root, filepath.FromSlash(tc.want))
			}
			if got != want || ok != (tc.want != "") {
				t.Errorf("got %q, %v; want %q", got, ok, want)
			}
		})
	}
}
