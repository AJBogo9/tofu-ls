// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	tfmod "github.com/opentofu/opentofu-schema/module"
	tfregistry "github.com/opentofu/opentofu-schema/registry"
	tfaddr "github.com/opentofu/registry-address"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/modules/decoder"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/ast"
	"github.com/zclconf/go-cty/cty"
)

type RootReaderMock struct{}

func (r RootReaderMock) InstalledModuleCalls(modPath string) (map[string]tfmod.InstalledModuleCall, error) {
	return nil, nil
}

func (r RootReaderMock) TofuVersion(modPath string) *version.Version {
	return nil
}

func (r RootReaderMock) InstalledModulePath(rootPath string, normalizedSource string) (string, bool) {
	return "", false
}

func TestSchemaModuleValidation_FullModule(t *testing.T) {
	ctx := context.Background()
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := state.NewModuleStore(gs.ProviderSchemas, gs.RegistryModules, gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}

	testData, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	modPath := filepath.Join(testData, "invalid-config")

	err = ms.Add(modPath)
	if err != nil {
		t.Fatal(err)
	}

	fs := filesystem.NewFilesystem(gs.DocumentStore)
	ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{
		Method:     "textDocument/didOpen",
		LanguageID: ilsp.OpenTofu.String(),
		URI:        "file:///test/variables.tf",
	})
	err = ParseModuleConfiguration(ctx, fs, ms, modPath)
	if err != nil {
		t.Fatal(err)
	}
	err = SchemaModuleValidation(ctx, ms, RootReaderMock{}, modPath)
	if err != nil {
		t.Fatal(err)
	}

	mod, err := ms.ModuleRecordByPath(modPath)
	if err != nil {
		t.Fatal(err)
	}

	expectedCount := 5
	diagsCount := mod.ModuleDiagnostics[ast.SchemaValidationSource].Count()
	if diagsCount != expectedCount {
		t.Fatalf("expected %d diagnostics, %d given", expectedCount, diagsCount)
	}
}

func TestSchemaModuleValidation_SingleFile(t *testing.T) {
	ctx := context.Background()
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := state.NewModuleStore(gs.ProviderSchemas, gs.RegistryModules, gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}

	testData, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	modPath := filepath.Join(testData, "invalid-config")

	err = ms.Add(modPath)
	if err != nil {
		t.Fatal(err)
	}

	fs := filesystem.NewFilesystem(gs.DocumentStore)
	ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{
		Method:     "textDocument/didChange",
		LanguageID: ilsp.OpenTofu.String(),
		URI:        "file:///test/variables.tf",
	})
	err = ParseModuleConfiguration(ctx, fs, ms, modPath)
	if err != nil {
		t.Fatal(err)
	}
	err = SchemaModuleValidation(ctx, ms, RootReaderMock{}, modPath)
	if err != nil {
		t.Fatal(err)
	}

	mod, err := ms.ModuleRecordByPath(modPath)
	if err != nil {
		t.Fatal(err)
	}

	expectedCount := 3
	diagsCount := mod.ModuleDiagnostics[ast.SchemaValidationSource].Count()
	if diagsCount != expectedCount {
		t.Fatalf("expected %d diagnostics, %d given", expectedCount, diagsCount)
	}
}

// TestSchemaModuleValidation_registryModuleInputs checks that the inputs
// of a module call are never rejected on the registry's word: its data
// can be incomplete (the registry lists no variables at all for
// terraform-aws-modules/key-pair/aws v2.1.1 and s3-bucket/aws v5.16.1)
// and arrives while the module is validated, so rejecting would make
// "Unexpected attribute" errors appear and disappear. The variables of a
// local or installed module are known, and its unknown inputs are still
// rejected. A required input that the registry lists is still reported
// when it is missing.
func TestSchemaModuleValidation_registryModuleInputs(t *testing.T) {
	testCases := []struct {
		name string
		// inputs are the registry's inputs of the module, nil when no
		// data is cached; a name ending in ! is required
		inputs []string
		// installed installs the module with the variable key_name only
		installed bool
		want      []string
	}{
		{"no registry data", nil, false, nil},
		{"registry data without inputs", []string{}, false, nil},
		{"registry data with some inputs", []string{"key_name"}, false, nil},
		{"registry data with every input", []string{"key_name", "tags"}, false, nil},
		{"registry data with a required input left out", []string{"key_name", "tags", "create!"}, false,
			[]string{`Required attribute "create" not specified: An attribute named "create" is required here`}},
		{"an installed module", []string{}, true,
			[]string{`Unexpected attribute: An attribute named "tags" is not expected here`}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{
				Method:     "textDocument/didOpen",
				LanguageID: ilsp.OpenTofu.String(),
			})
			gs, err := globalState.NewStateStore()
			if err != nil {
				t.Fatal(err)
			}
			ms, err := state.NewModuleStore(gs.ProviderSchemas, gs.RegistryModules, gs.ChangeStore)
			if err != nil {
				t.Fatal(err)
			}
			modPath := t.TempDir()
			childPath := filepath.Join(modPath, "child")
			installedPath := filepath.Join(modPath, ".terraform", "modules", "reg")
			for _, dir := range []string{childPath, installedPath} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				filepath.Join(modPath, "main.tf"): `module "reg" {
  source  = "terraform-aws-modules/key-pair/aws"
  version = "~> 2.0"

  key_name = "x"
  tags     = {}
}

module "loc" {
  source  = "./child"
  known   = 1
  unknown = 2
}
`,
				filepath.Join(childPath, "main.tf"):     "variable \"known\" {}\n",
				filepath.Join(installedPath, "main.tf"): "variable \"key_name\" {}\n",
			}
			for name, src := range files {
				if err := os.WriteFile(name, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs := filesystem.NewFilesystem(gs.DocumentStore)
			for _, dir := range []string{modPath, childPath, installedPath} {
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
			if tc.inputs != nil {
				addr, err := tfaddr.ParseModuleSource("terraform-aws-modules/key-pair/aws")
				if err != nil {
					t.Fatal(err)
				}
				inputs := make([]tfregistry.Input, 0, len(tc.inputs))
				for _, name := range tc.inputs {
					required := strings.HasSuffix(name, "!")
					inputs = append(inputs, tfregistry.Input{Name: strings.TrimSuffix(name, "!"), Type: cty.DynamicPseudoType, Required: required})
				}
				if err := gs.RegistryModules.Cache(addr, version.Must(version.NewVersion("2.1.1")), inputs, nil); err != nil {
					t.Fatal(err)
				}
			}

			var roots fdecoder.RootReader = RootReaderMock{}
			if tc.installed {
				roots = installedRootReader{source: "registry.opentofu.org/terraform-aws-modules/key-pair/aws", dir: filepath.Join(".terraform", "modules", "reg")}
			}
			if err := SchemaModuleValidation(ctx, ms, roots, modPath); err != nil {
				t.Fatal(err)
			}
			mod, err := ms.ModuleRecordByPath(modPath)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range mod.ModuleDiagnostics[ast.SchemaValidationSource]["main.tf"] {
				got = append(got, d.Summary+": "+d.Detail)
			}
			sort.Strings(got)
			want := append([]string{`Unexpected attribute: An attribute named "unknown" is not expected here`}, tc.want...)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("expected:\n%s\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

// installedRootReader has the module of one source installed in dir.
type installedRootReader struct {
	RootReaderMock
	source, dir string
}

func (r installedRootReader) InstalledModulePath(rootPath string, normalizedSource string) (string, bool) {
	return r.dir, normalizedSource == r.source
}
