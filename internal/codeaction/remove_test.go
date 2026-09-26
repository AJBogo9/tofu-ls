// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestRemoveUnusedVariable(t *testing.T) {
	testCases := []struct {
		name     string
		files    files
		doc      string
		needle   string
		data     map[string]interface{}
		settings []string // file:attribute found in the file, as the env returns them
		title    string
		edits    []string
		want     files
	}{
		{
			name: "block between two others, with its tfvars value",
			files: files{
				"variables.tf": `variable "a" {}

variable "unused" {
  type    = string
  default = "x"
}

variable "b" {}
`,
				"terraform.tfvars": `a      = 1
unused = "y"
`,
			},
			doc:      "variables.tf",
			needle:   `"unused"`,
			data:     map[string]interface{}{"name": "unused"},
			settings: []string{"terraform.tfvars:unused"},
			title:    `Remove unused variable "unused" and where it is set (terraform.tfvars)`,
			edits: []string{
				`terraform.tfvars 1:1-3:1 "a = 1\n"`,
				`variables.tf 3:1-8:1 ""`,
			},
			want: files{
				"variables.tf": `variable "a" {}

variable "b" {}
`,
				"terraform.tfvars": `a = 1
`,
			},
		},
		{
			name: "last block of the file, name from the range, with its comment",
			files: files{
				"main.tf": `resource "terraform_data" "x" {}

# Not read by anything.
variable "unused" {}
`,
			},
			doc:    "main.tf",
			needle: `"unused"`,
			title:  `Remove unused variable "unused"`,
			edits:  []string{`main.tf 2:1-5:1 ""`},
			want: files{
				"main.tf": `resource "terraform_data" "x" {}
`,
			},
		},
		{
			name: "a comment shared with the next block stays",
			files: files{
				"main.tf": `# Inputs
variable "unused" {}
variable "used" {}
`,
			},
			doc:    "main.tf",
			needle: `"unused"`,
			title:  `Remove unused variable "unused"`,
			edits:  []string{`main.tf 2:1-3:1 ""`},
			want: files{
				"main.tf": `# Inputs
variable "used" {}
`,
			},
		},
		{
			name: "declared again in an override file",
			files: files{
				"variables.tf": "variable \"unused\" {\n  default = 1\n}\n",
				"override.tf":  "variable \"unused\" {\n  default = 2\n}\n\nvariable \"other\" {}\n",
			},
			doc:    "variables.tf",
			needle: `"unused"`,
			data:   map[string]interface{}{"name": "unused"},
			title:  `Remove unused variable "unused"`,
			edits: []string{
				`override.tf 1:1-5:1 ""`,
				`variables.tf 1:1-4:1 ""`,
			},
			want: files{
				"variables.tf": "",
				"override.tf":  "variable \"other\" {}\n",
			},
		},
		{
			name: "set by a module call, a test run and a var file in a subdirectory",
			files: files{
				"mod/variables.tf": "variable \"unused\" {}\n",
				"main.tf":          "module \"m\" {\n  source = \"./mod\"\n  unused = 1\n  other  = 2\n}\n",
				"mod/tests/a.tftest.hcl": `run "one" {
  variables {
    unused = 1
  }
}
`,
				"mod/envs/prod.tfvars": "unused = 3\nother  = 4\n",
			},
			doc:    "mod/variables.tf",
			needle: `"unused"`,
			data:   map[string]interface{}{"name": "unused"},
			settings: []string{
				"main.tf:unused",
				"mod/tests/a.tftest.hcl:variables",
				"mod/envs/prod.tfvars:unused",
			},
			title: `Remove unused variable "unused" and where it is set (../main.tf, envs/prod.tfvars, tests/a.tftest.hcl)`,
			edits: []string{
				`main.tf 3:1-4:1 ""`,
				`mod/envs/prod.tfvars 1:1-3:1 "other = 4\n"`,
				`mod/tests/a.tftest.hcl 2:1-5:1 ""`,
				`mod/variables.tf 1:1-2:1 ""`,
			},
			want: files{
				"mod/variables.tf":       "",
				"main.tf":                "module \"m\" {\n  source = \"./mod\"\n  other  = 2\n}\n",
				"mod/tests/a.tftest.hcl": "run \"one\" {\n}\n",
				"mod/envs/prod.tfvars":   "other = 4\n",
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			env := tc.files.newEnv()
			env.VariableSettings = func(dir, name string) ([]Location, bool) {
				if dir != filepath.Dir(filepath.Join(root, tc.doc)) || name != "unused" {
					t.Fatalf("unexpected settings request for %s in %s", name, dir)
				}
				locs := make([]Location, 0)
				for _, s := range tc.settings {
					file, attr, _ := strings.Cut(s, ":")
					locs = append(locs, Location{File: filepath.Join(root, file), Range: settingRange(t, tc.files, file, attr)})
				}
				return locs, true
			}
			diag := Diagnostic{Code: CodeUnusedVariable, Data: tc.data, Range: tc.files.rangeOf(t, tc.doc, tc.needle, 1)}
			actions := QuickFixes(env, tc.files.doc(tc.doc), diag)
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
			if !a.Preferred || a.Kind != KindQuickFix {
				t.Errorf("expected a preferred quick fix, got %#v", a)
			}
		})
	}
}

func TestRemoveUnusedVariable_settingsUnknown(t *testing.T) {
	fsys := files{"variables.tf": "variable \"unused\" {}\n"}
	env := fsys.newEnv()
	env.VariableSettings = func(dir, name string) ([]Location, bool) { return nil, false }
	diag := Diagnostic{Code: CodeUnusedVariable, Range: fsys.rangeOf(t, "variables.tf", `"unused"`, 1)}
	if actions := QuickFixes(env, fsys.doc("variables.tf"), diag); len(actions) != 0 {
		t.Fatalf("expected no fix when the settings cannot be read, got %#v", actions)
	}
}

func TestRemoveUnusedVariable_syntaxErrorInModule(t *testing.T) {
	fsys := files{
		"variables.tf": "variable \"unused\" {}\n",
		"main.tf":      "resource \"terraform_data\" \"x\" {\n",
	}
	diag := Diagnostic{Code: CodeUnusedVariable, Range: fsys.rangeOf(t, "variables.tf", `"unused"`, 1)}
	if actions := QuickFixes(fsys.newEnv(), fsys.doc("variables.tf"), diag); len(actions) != 0 {
		t.Fatalf("expected no fix while a module file has syntax errors, got %#v", actions)
	}
}

// settingRange is the range of the attribute or block called name in a
// file: a top-level attribute, else the first one found at any depth.
func settingRange(t *testing.T, fsys files, file, name string) hcl.Range {
	t.Helper()
	body, ok := parse(filepath.Join(root, file), []byte(fsys[file]))
	if !ok {
		t.Fatalf("%s does not parse", file)
	}
	var find func(body *hclsyntax.Body) (hcl.Range, bool)
	find = func(body *hclsyntax.Body) (hcl.Range, bool) {
		if attr, ok := body.Attributes[name]; ok {
			return attr.SrcRange, true
		}
		for _, block := range body.Blocks {
			if block.Type == name {
				return block.Range(), true
			}
			if rng, ok := find(block.Body); ok {
				return rng, true
			}
		}
		return hcl.Range{}, false
	}
	rng, ok := find(body)
	if !ok {
		t.Fatalf("%s not in %s", name, file)
	}
	return rng
}

func TestRemoveUnusedLocal(t *testing.T) {
	testCases := []struct {
		name   string
		files  files
		doc    string
		needle string
		data   map[string]interface{}
		title  string
		edits  []string
		want   files
	}{
		{
			name: "one of several",
			files: files{
				"locals.tf": `locals {
  a      = 1
  unused = 2
  b      = 3
}
`,
			},
			doc:    "locals.tf",
			needle: "unused",
			data:   map[string]interface{}{"name": "unused"},
			title:  `Remove unused local "unused"`,
			edits:  []string{`locals.tf 2:1-5:1 "  a = 1\n  b = 3\n"`},
			want: files{
				"locals.tf": `locals {
  a = 1
  b = 3
}
`,
			},
		},
		{
			name: "the last of its block takes the block",
			files: files{
				"main.tf": `locals {
  a = 1
}

locals {
  unused = {
    k = "v"
  }
}
`,
			},
			doc:    "main.tf",
			needle: "unused",
			title:  `Remove unused local "unused"`,
			edits:  []string{`main.tf 4:1-10:1 ""`},
			want: files{
				"main.tf": `locals {
  a = 1
}
`,
			},
		},
		{
			name: "a one-line block",
			files: files{
				"main.tf": "locals { unused = 1 }\n\noutput \"o\" {\n  value = 1\n}\n",
			},
			doc:    "main.tf",
			needle: "unused",
			title:  `Remove unused local "unused"`,
			edits:  []string{`main.tf 1:1-3:1 ""`},
			want: files{
				"main.tf": "output \"o\" {\n  value = 1\n}\n",
			},
		},
		{
			name: "a blank line stays between the neighbours",
			files: files{
				"main.tf": "locals {\n  a = 1\n\n  unused = 2\n\n  b = 3\n}\n",
			},
			doc:    "main.tf",
			needle: "unused",
			title:  `Remove unused local "unused"`,
			edits:  []string{`main.tf 4:1-6:1 ""`},
			want: files{
				"main.tf": "locals {\n  a = 1\n\n  b = 3\n}\n",
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			diag := Diagnostic{Code: CodeUnusedLocal, Data: tc.data, Range: tc.files.rangeOf(t, tc.doc, tc.needle, 1)}
			actions := QuickFixes(tc.files.newEnv(), tc.files.doc(tc.doc), diag)
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
			if !a.Preferred {
				t.Errorf("expected a preferred fix")
			}
		})
	}
}

func TestRemoveUnusedDataSource(t *testing.T) {
	testCases := []struct {
		name   string
		files  files
		needle string
		data   map[string]interface{}
		title  string
		edits  []string
		want   files
	}{
		{
			name: "top level, from the address",
			files: files{
				"main.tf": `data "local_file" "unused" {
  filename = "a.txt"
}

resource "terraform_data" "x" {}
`,
			},
			needle: `"unused"`,
			data:   map[string]interface{}{"address": "data.local_file.unused"},
			title:  "Remove unused data source data.local_file.unused",
			edits:  []string{`main.tf 1:1-5:1 ""`},
			want: files{
				"main.tf": `resource "terraform_data" "x" {}
`,
			},
		},
		{
			name: "scoped to a check block, from the range",
			files: files{
				"main.tf": `check "health" {
  data "http" "unused" {
    url = "https://example.com"
  }

  assert {
    condition     = true
    error_message = "never"
  }
}
`,
			},
			needle: `"http"`,
			title:  "Remove unused data source data.http.unused",
			edits:  []string{`main.tf 2:1-6:1 ""`},
			want: files{
				"main.tf": `check "health" {
  assert {
    condition     = true
    error_message = "never"
  }
}
`,
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			diag := Diagnostic{Code: CodeUnusedDataSource, Data: tc.data, Range: tc.files.rangeOf(t, "main.tf", tc.needle, 1)}
			actions := QuickFixes(tc.files.newEnv(), tc.files.doc("main.tf"), diag)
			expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
		})
	}
}
