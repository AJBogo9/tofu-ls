// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/ast"
)

func TestUnusedSymbols(t *testing.T) {
	testCases := []struct {
		name     string
		files    map[string]string
		expected []string
	}{
		{
			"unused variable, tfvars-only variable and unused local",
			map[string]string{
				"main.tf": `variable "used" {}
variable "unused" {}
variable "only_tfvars" {}
locals {
  dead = 1
  live = var.used
}
output "o" {
  value = local.live
}
`,
				"terraform.tfvars": `only_tfvars = "x"
`,
			},
			[]string{
				`main.tf:5:3 Local value "dead" is declared but not used`,
				`main.tf:2:10 Variable "unused" is declared but not used in this module`,
				`main.tf:3:10 Variable "only_tfvars" is declared but not used in this module (only set in terraform.tfvars)`,
			},
		},
		{
			"a file with syntax errors means no report",
			map[string]string{
				"main.tf":   `variable "unused" {}`,
				"broken.tf": `resource "x" "y" {`,
			},
			[]string{},
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
			for name, src := range tc.files {
				if err := os.WriteFile(filepath.Join(modPath, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := ms.Add(modPath); err != nil {
				t.Fatal(err)
			}

			fs := filesystem.NewFilesystem(gs.DocumentStore)
			ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{
				Method:     "textDocument/didOpen",
				LanguageID: ilsp.OpenTofu.String(),
				URI:        "file:///test/main.tf",
			})
			if err := ParseModuleConfiguration(ctx, fs, ms, modPath); err != nil {
				t.Fatal(err)
			}
			if err := UnusedSymbols(ctx, fs, ms, modPath); err != nil {
				t.Fatal(err)
			}

			mod, err := ms.ModuleRecordByPath(modPath)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for file, diags := range mod.ModuleDiagnostics[ast.UnusedSymbolsSource] {
				for _, d := range diags {
					got = append(got, fmt.Sprintf("%s:%d:%d %s", file, d.Subject.Start.Line, d.Subject.Start.Column, d.Summary))
				}
			}
			sort.Strings(got)
			expected := append([]string{}, tc.expected...)
			sort.Strings(expected)
			if strings.Join(got, "\n") != strings.Join(expected, "\n") {
				t.Fatalf("expected:\n%s\ngot:\n%s", strings.Join(expected, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}
