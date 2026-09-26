// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestValidateName(t *testing.T) {
	testCases := []struct {
		kind    SymbolKind
		name    string
		wantErr string
	}{
		{KindVariable, "tier", ""},
		{KindResource, "web-1", ""},
		{KindLocal, "_private", ""},
		{KindVariable, "9lives", "not a valid name"},
		{KindModule, "two words", "not a valid name"},
		{KindOutput, "", "not a valid name"},
		{KindVariable, "count", "reserved"},
		{KindVariable, "for_each", "reserved"},
		{KindLocal, "count", ""},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s-%s", i, tc.kind, tc.name), func(t *testing.T) {
			err := ValidateName(tc.kind, tc.name)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %s", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestWalkBodyTraversals(t *testing.T) {
	src := `resource "local_file" "a" {
  provider = local.secondary
  content  = "${local.secondary}-${var.stage}"
  lifecycle {
    replace_triggered_by = [terraform_data.rev]
  }
}
module "m" {
  source    = "./m"
  providers = { local = local.secondary }
  name      = local.secondary
}
moved {
  from = terraform_data.old
  to   = terraform_data.rev
}
variable "stage" {
  validation {
    condition = contains(["a"], var.stage)
  }
}
`
	f, diags := hclsyntax.ParseConfig([]byte(src), "main.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	testCases := []struct {
		name     string
		prefix   []string
		nameIdx  int
		expected []string
	}{
		{
			"local skips provider meta-arguments",
			[]string{"local", "secondary"}, 1,
			[]string{"3:23-3:32", "11:21-11:30"},
		},
		{
			"variable inside a validation condition",
			[]string{"var", "stage"}, 1,
			[]string{"3:40-3:45", "19:37-19:42"},
		},
		{
			"resource in a nested block and in a moved block",
			[]string{"terraform_data", "rev"}, 1,
			[]string{"5:44-5:47", "15:25-15:28 (moved)"},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			got := make([]string, 0)
			err := walkBodyTraversals(f.Body.(*hclsyntax.Body), "", false, func(tr hcl.Traversal, inMoved bool) error {
				rng, ok := stepRangeIfPrefix(tr, tc.prefix, tc.nameIdx)
				if !ok {
					return nil
				}
				s := fmt.Sprintf("%d:%d-%d:%d", rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column)
				if inMoved {
					s += " (moved)"
				}
				got = append(got, s)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(sorted(got), ",") != strings.Join(sorted(tc.expected), ",") {
				t.Fatalf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestUnquotedLabelRange(t *testing.T) {
	src := []byte(`variable "stage" {}
resource aws_instance web {}
`)
	testCases := []struct {
		name     string
		rng      hcl.Range
		label    string
		expected string
		ok       bool
	}{
		{"quoted label", hcl.Range{Start: hcl.Pos{Line: 1, Column: 10, Byte: 9}, End: hcl.Pos{Line: 1, Column: 17, Byte: 16}}, "stage", "1:11-1:16", true},
		{"unquoted label", hcl.Range{Start: hcl.Pos{Line: 2, Column: 23, Byte: 42}, End: hcl.Pos{Line: 2, Column: 26, Byte: 45}}, "web", "2:23-2:26", true},
		{"different name", hcl.Range{Start: hcl.Pos{Line: 1, Column: 10, Byte: 9}, End: hcl.Pos{Line: 1, Column: 17, Byte: 16}}, "other", "", false},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			rng, ok := unquotedLabelRange(src, tc.rng, tc.label)
			if ok != tc.ok {
				t.Fatalf("expected ok=%t, got %t", tc.ok, ok)
			}
			if !ok {
				return
			}
			got := fmt.Sprintf("%d:%d-%d:%d", rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column)
			if got != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

func TestMovedBlock(t *testing.T) {
	from := lang.Address{lang.RootStep{Name: "module"}, lang.AttrStep{Name: "app"}}
	to := lang.Address{lang.RootStep{Name: "module"}, lang.AttrStep{Name: "web"}}

	testCases := []struct {
		name     string
		src      string
		expected string
	}{
		{"file ending with a newline", "module \"app\" {}\n", "\nmoved {\n  from = module.app\n  to   = module.web\n}\n"},
		{"file without a final newline", "module \"app\" {}", "\n\nmoved {\n  from = module.app\n  to   = module.web\n}\n"},
		{"file with CRLF line endings", "module \"app\" {\r\n}\r\n", "\r\nmoved {\r\n  from = module.app\r\n  to   = module.web\r\n}\r\n"},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			got := movedBlock([]byte(tc.src), from, to)
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
			// the result must parse
			_, diags := hclsyntax.ParseConfig([]byte(tc.src+got), "main.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
		})
	}
}

func TestIsInstalledModule(t *testing.T) {
	testCases := []struct {
		dir  string
		want bool
	}{
		{"/work/showcase", false},
		{"/work/showcase/modules/app", false},
		{"/work/showcase/.terraform/modules/remote", true},
		{"/work/showcase/.terraform/modules/remote/modules/sub", true},
		{"/work/showcase/terraform/modules/x", false},
	}
	for _, tc := range testCases {
		t.Run(tc.dir, func(t *testing.T) {
			if got := isInstalledModule(filepath.FromSlash(tc.dir)); got != tc.want {
				t.Fatalf("isInstalledModule(%q) = %t, want %t", tc.dir, got, tc.want)
			}
		})
	}
}

func TestModuleOutputStepRange(t *testing.T) {
	src := `locals {
  a = module.app.port
  b = module.app[0].port
  c = module.app["k"].port
  d = module.app[*].port
  e = module.app[local.k].port
  f = module.other.port
  g = module.app.port_2
  h = module.app
}
`
	f, diags := hclsyntax.ParseConfig([]byte(src), "main.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	got := make([]string, 0)
	err := walkBodyTraversals(f.Body.(*hclsyntax.Body), "", false, func(tr hcl.Traversal, _ bool) error {
		if rng, ok := moduleOutputStepRange(tr, "app", "port"); ok {
			got = append(got, fmt.Sprintf("%d:%d-%d:%d", rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"2:18-2:22", "3:21-3:25", "4:23-4:27", "5:21-5:25", "6:27-6:31"}
	if strings.Join(sorted(got), ",") != strings.Join(sorted(expected), ",") {
		t.Fatalf("expected %v, got %v", expected, got)
	}
}

func TestFileCache_syntaxErrorNamesTheFile(t *testing.T) {
	root := filepath.FromSlash("/work/showcase")
	fc := newFileCache(Env{ReadFile: func(path string) ([]byte, error) {
		return []byte("resource \"x\" {\n"), nil
	}})
	fc.base = root
	_, _, err := fc.body(filepath.Join(root, "modules", "app", "main.tf"))
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	want := filepath.Join("modules", "app", "main.tf") + " has syntax errors; fix them before renaming"
	if err.Error() != want {
		t.Fatalf("expected %q, got %q", want, err.Error())
	}
}

func TestFileCache_syntaxErrorNamesTheFileInTheWorkspace(t *testing.T) {
	workspace := filepath.FromSlash("/work")
	child := filepath.Join(workspace, "modules", "app")
	relPath := func(path string) string {
		if rel, err := filepath.Rel(workspace, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		return ""
	}
	testCases := []struct {
		name string
		file string
		want string
	}{
		// renaming from the root a symbol the child declares: the base is
		// the child, the message still names the child's file in full
		{"in the workspace", filepath.Join(child, "main.tf"), filepath.Join("modules", "app", "main.tf")},
		// outside every workspace folder: relative to the base
		{"outside the workspace", filepath.FromSlash("/other/app/main.tf"), filepath.Join("..", "..", "..", "other", "app", "main.tf")},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFileCache(Env{
				ReadFile: func(path string) ([]byte, error) {
					return []byte("resource \"x\" {\n"), nil
				},
				RelPath: relPath,
			})
			fc.base = child
			_, _, err := fc.body(tc.file)
			if err == nil {
				t.Fatal("expected a syntax error")
			}
			want := tc.want + " has syntax errors; fix them before renaming"
			if err.Error() != want {
				t.Fatalf("expected %q, got %q", want, err.Error())
			}
		})
	}
}
