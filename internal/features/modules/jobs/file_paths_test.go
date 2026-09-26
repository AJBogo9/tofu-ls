// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	modast "github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/ast"
)

func TestMissingPathFiles(t *testing.T) {
	mainTf := `locals {
  ok        = templatefile("${path.module}/templates/a.tftpl", {})
  missing   = templatefile("${path.module}/templates/missing.tftpl", {})
  bare      = file("data/missing.json")
  bare_ok   = file("data/present.json")
  maybe     = fileexists("${path.module}/nope.txt")
  set_ok    = fileset(path.module, "*.tftpl")
  set_gone  = fileset("${path.module}/nodir", "*")
  dir       = file("${path.module}/templates")
  from_var  = file(var.path)
  core      = core::filebase64("${path.module}/gone.bin")
}
`
	testCases := []struct {
		name     string
		called   bool
		expected []string
	}{
		{
			"root module",
			false,
			[]string{
				`11:32 template-file-missing File "gone.bin" not found`,
				`3:28 template-file-missing File "templates/missing.tftpl" not found`,
				`4:20 template-file-missing File "data/missing.json" not found`,
				`8:23 template-file-missing Directory "nodir" not found`,
				`9:20 template-file-missing "templates" is a directory, not a file`,
			},
		},
		{
			// paths relative to the root module resolve from the caller
			"called module",
			true,
			[]string{
				`11:32 template-file-missing File "gone.bin" not found`,
				`3:28 template-file-missing File "templates/missing.tftpl" not found`,
				`8:23 template-file-missing Directory "nodir" not found`,
				`9:20 template-file-missing "templates" is a directory, not a file`,
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			ctx := context.Background()
			gs, err := globalState.NewStateStore()
			if err != nil {
				t.Fatal(err)
			}
			ms, err := state.NewModuleStore(gs.ProviderSchemas, gs.RegistryModules, gs.ChangeStore)
			if err != nil {
				t.Fatal(err)
			}

			modPath := t.TempDir()
			for name, src := range map[string]string{
				"main.tf":           mainTf,
				"templates/a.tftpl": "hello",
				"data/present.json": "{}",
				"z_unrelated.tftpl": "",
			} {
				p := filepath.Join(modPath, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := ms.Add(modPath); err != nil {
				t.Fatal(err)
			}
			f, diags := hclsyntax.ParseConfig([]byte(mainTf), "main.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			if err := ms.UpdateParsedModuleFiles(modPath, modast.ModFiles{"main.tf": f}, nil); err != nil {
				t.Fatal(err)
			}

			fs := filesystem.NewFilesystem(gs.DocumentStore)
			err = MissingPathFiles(ctx, fs, ms, modPath, func() bool { return tc.called })
			if err != nil {
				t.Fatal(err)
			}

			mod, err := ms.ModuleRecordByPath(modPath)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for _, d := range mod.ModuleDiagnostics[ast.FilePathsSource]["main.tf"] {
				code := d.Extra.(ilsp.CodedDiagnostic)
				got = append(got, fmt.Sprintf("%d:%d %s %s", d.Subject.Start.Line, d.Subject.Start.Column, code.Code, d.Summary))
				if code.Data["path"] == "" || code.Data["function"] == "" {
					t.Fatalf("missing data: %#v", code.Data)
				}
			}
			sort.Strings(got)
			if diff := cmp.Diff(tc.expected, got); diff != "" {
				t.Fatalf("unexpected diagnostics: %s", diff)
			}
		})
	}
}

func TestMissingPathFiles_clearsWhenFixed(t *testing.T) {
	ctx := context.Background()
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := state.NewModuleStore(gs.ProviderSchemas, gs.RegistryModules, gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	modPath := t.TempDir()
	if err := ms.Add(modPath); err != nil {
		t.Fatal(err)
	}
	fs := filesystem.NewFilesystem(gs.DocumentStore)
	check := func(src string) int {
		f, _ := hclsyntax.ParseConfig([]byte(src), "main.tf", hcl.InitialPos)
		if err := ms.UpdateParsedModuleFiles(modPath, modast.ModFiles{"main.tf": f}, nil); err != nil {
			t.Fatal(err)
		}
		if err := MissingPathFiles(ctx, fs, ms, modPath, nil); err != nil {
			t.Fatal(err)
		}
		mod, err := ms.ModuleRecordByPath(modPath)
		if err != nil {
			t.Fatal(err)
		}
		return len(mod.ModuleDiagnostics[ast.FilePathsSource]["main.tf"])
	}

	if n := check("locals {\n  a = file(\"${path.module}/a.txt\")\n}\n"); n != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", n)
	}
	if err := os.WriteFile(filepath.Join(modPath, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := check("locals {\n  a = file(\"${path.module}/a.txt\")\n}\n"); n != 0 {
		t.Fatalf("expected the diagnostic cleared, got %d", n)
	}
}
