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

	"github.com/hashicorp/go-version"
	tfmod "github.com/opentofu/opentofu-schema/module"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	"github.com/opentofu/tofu-ls/internal/job"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/settings"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/tofu/ast"
)

type rootReaderStub struct{}

func (rootReaderStub) InstalledModuleCalls(string) (map[string]tfmod.InstalledModuleCall, error) {
	return nil, nil
}
func (rootReaderStub) TofuVersion(string) *version.Version { return nil }
func (rootReaderStub) InstalledModulePath(string, string) (string, bool) {
	return "", false
}

func TestSemanticValidation(t *testing.T) {
	allOn := settings.ValidationOptions{
		EnableEnhancedValidation: true,
		DuplicateDeclarations:    true,
		UnresolvedReferences:     true,
		UnknownResourceTypes:     true,
		VariableTypes:            true,
		Tfvars:                   true,
		StaticValues:             true,
		OperandTypes:             true,
		Installation:             true,
		UnusedDataSources:        true,
		InterpolationOnly:        true,
	}
	testCases := []struct {
		name     string
		files    map[string]string
		opts     settings.ValidationOptions
		expected []string
	}{
		{
			"module and tfvars diagnostics, with outputs of a called module",
			map[string]string{
				"main.tf": `variable "typed" { type = number }
variable "typed2" {}
variable "typed2" {}
module "child" {
  source = "./child"
}
output "o" {
  value = [var.typed, var.typed2, module.child.out, module.child.nope, random_pet.nope.id]
}
`,
				"child/main.tf":    `output "out" { value = 1 }`,
				"terraform.tfvars": "typed      = \"not-a-number\"\nundeclared = 1\n",
			},
			allOn,
			[]string{
				`main.tf:3:10 duplicate-declaration Duplicate variable declaration`,
				`main.tf:8:53 unresolved-reference Unsupported attribute`,
				`main.tf:8:72 unresolved-reference Reference to undeclared resource`,
				`terraform.tfvars:1:14 tfvars-type-mismatch Invalid value for input variable`,
				`terraform.tfvars:2:1 tfvars-undeclared-variable Value for undeclared variable`,
			},
		},
		{
			"families switched off report nothing",
			map[string]string{
				"main.tf":          `variable "v" {}` + "\n" + `variable "v" {}`,
				"terraform.tfvars": "undeclared = 1\n",
			},
			settings.ValidationOptions{EnableEnhancedValidation: true},
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
				path := filepath.Join(modPath, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs := filesystem.NewFilesystem(gs.DocumentStore)
			ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{
				Method:     "textDocument/didOpen",
				LanguageID: ilsp.OpenTofu.String(),
				URI:        "file:///test/main.tf",
			})
			for _, dir := range []string{filepath.Join(modPath, "child"), modPath} {
				if _, err := os.Stat(dir); err != nil {
					continue
				}
				if err := ms.Add(dir); err != nil {
					t.Fatal(err)
				}
				if err := ParseModuleConfiguration(ctx, fs, ms, dir); err != nil {
					t.Fatal(err)
				}
				if err := LoadModuleMetadata(ctx, ms, dir); err != nil {
					t.Fatal(err)
				}
			}

			if err := SemanticValidation(ctx, fs, ms, rootReaderStub{}, gs.ProviderSchemas, modPath, tc.opts, nil); err != nil {
				t.Fatal(err)
			}

			mod, err := ms.ModuleRecordByPath(modPath)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for file, diags := range mod.ModuleDiagnostics[ast.SemanticValidationSource] {
				for _, d := range diags {
					code := ""
					if coded, ok := d.Extra.(ilsp.CodedDiagnostic); ok {
						code = coded.Code
					}
					got = append(got, fmt.Sprintf("%s:%d:%d %s %s", file, d.Subject.Start.Line, d.Subject.Start.Column, code, d.Summary))
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

type inputsStub struct{ in staticval.Inputs }

func (s *inputsStub) Inputs(string) staticval.Inputs { return s.in }

// TestSemanticValidation_varFiles checks a -var-file file of the selected
// environment in a subdirectory: its value is validated and its names
// checked, and its diagnostics are cleared once it is no longer chosen.
func TestSemanticValidation_varFiles(t *testing.T) {
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
	files := map[string]string{
		"main.tf": `variable "stage" {
  type    = string
  default = "dev"
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod."
  }
}
`,
		"envs/qa.tfvars": "stage = \"qa\"\nundeclared = 1\n",
	}
	for name, src := range files {
		path := filepath.Join(modPath, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := filesystem.NewFilesystem(gs.DocumentStore)
	ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{
		Method:     "textDocument/didOpen",
		LanguageID: ilsp.OpenTofu.String(),
		URI:        "file:///test/main.tf",
	})
	if err := ms.Add(modPath); err != nil {
		t.Fatal(err)
	}
	if err := ParseModuleConfiguration(ctx, fs, ms, modPath); err != nil {
		t.Fatal(err)
	}
	if err := LoadModuleMetadata(ctx, ms, modPath); err != nil {
		t.Fatal(err)
	}
	opts := settings.ValidationOptions{EnableEnhancedValidation: true, Tfvars: true, Conditions: true}
	inputs := &inputsStub{in: staticval.Inputs{VarFiles: []string{"envs/qa.tfvars"}}}

	report := func() string {
		mod, err := ms.ModuleRecordByPath(modPath)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0)
		for file, diags := range mod.ModuleDiagnostics[ast.SemanticValidationSource] {
			if len(diags) == 0 {
				got = append(got, fmt.Sprintf("%s cleared", file))
			}
			for _, d := range diags {
				code := ""
				if coded, ok := d.Extra.(ilsp.CodedDiagnostic); ok {
					code = coded.Code
				}
				got = append(got, fmt.Sprintf("%s:%d:%d %s %s", file, d.Subject.Start.Line, d.Subject.Start.Column, code, d.Detail))
			}
		}
		sort.Strings(got)
		return strings.Join(got, "\n")
	}

	ctx = job.WithIgnoreState(ctx, true)
	if err := SemanticValidation(ctx, fs, ms, rootReaderStub{}, gs.ProviderSchemas, modPath, opts, inputs); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`envs/qa.tfvars:1:9 validation-failed The stage must be dev or prod.`,
		`envs/qa.tfvars:2:1 tfvars-undeclared-variable The module does not declare a variable named "undeclared" but a value was found in file "envs/qa.tfvars". If you meant to use this value, add a "variable" block to the configuration.`,
		`main.tf cleared`,
	}, "\n")
	if got := report(); got != want {
		t.Fatalf("expected:\n%s\ngot:\n%s", want, got)
	}

	inputs.in = staticval.Inputs{}
	if err := SemanticValidation(ctx, fs, ms, rootReaderStub{}, gs.ProviderSchemas, modPath, opts, inputs); err != nil {
		t.Fatal(err)
	}
	want = "envs/qa.tfvars cleared\nmain.tf cleared"
	if got := report(); got != want {
		t.Fatalf("expected:\n%s\ngot:\n%s", want, got)
	}
}
