// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
)

type osFS struct{}

func (osFS) Open(name string) (fs.File, error)          { return os.Open(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }

func TestParseTerragruntFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"terragrunt.hcl":       "inputs = {}\n",
		"root.hcl":             "locals {}\n",
		"terragrunt.stack.hcl": "unit \"a\" {\n  source = \"x\"\n  path = \"a\"\n}\n",
		"account.hcl":          "locals {}\n",
		"env.hcl":              "locals {}\n",
		".terraform.lock.hcl":  "provider \"x\" {}\n",
		"main.tf":              "variable \"x\" {}\n",
		"broken.hcl":           "locals {\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testCases := []struct {
		name  string
		open  map[string]string
		files map[string]string
		diags map[string]int
	}{
		{
			"by name only: other .hcl files are left alone",
			nil,
			map[string]string{"terragrunt.hcl": "terragrunt", "root.hcl": "terragrunt", "terragrunt.stack.hcl": "terragrunt-stack"},
			map[string]int{"terragrunt.hcl": 0, "root.hcl": 0, "terragrunt.stack.hcl": 0},
		},
		{
			"a file open as Terragrunt joins, whatever its name; one open as another language leaves",
			map[string]string{"account.hcl": "terragrunt", "broken.hcl": "terragrunt-stack", "root.hcl": ""},
			map[string]string{"terragrunt.hcl": "terragrunt", "account.hcl": "terragrunt", "broken.hcl": "terragrunt-stack", "terragrunt.stack.hcl": "terragrunt-stack"},
			map[string]int{"terragrunt.hcl": 0, "account.hcl": 0, "broken.hcl": 1, "terragrunt.stack.hcl": 0},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files, languages, diags, err := ParseTerragruntFiles(osFS{}, dir, tc.open)
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string)
			for name, language := range languages {
				got[name.String()] = language
				if _, ok := files[name]; !ok {
					t.Errorf("%s has a language but no file", name)
				}
			}
			if diff := cmp.Diff(tc.files, got); diff != "" {
				t.Errorf("languages: %s", diff)
			}
			gotDiags := make(map[string]int)
			for name, d := range diags {
				gotDiags[name.String()] = len(d)
			}
			if diff := cmp.Diff(tc.diags, gotDiags); diff != "" {
				names := make([]string, 0)
				for n := range gotDiags {
					names = append(names, n)
				}
				sort.Strings(names)
				t.Errorf("diagnostics: %s (%v)", diff, names)
			}
		})
	}
}
