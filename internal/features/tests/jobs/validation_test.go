// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	tfmod "github.com/opentofu/opentofu-schema/module"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// noModules knows no module: provider blocks keep the core schema
type noModules struct{}

func (noModules) LocalModuleMeta(modPath string) (*tfmod.Meta, error) {
	return nil, fmt.Errorf("%s: not indexed", modPath)
}

func (noModules) PathContext(path lang.Path) (*decoder.PathContext, error) {
	return nil, fmt.Errorf("%s: not indexed", path.Path)
}

func (noModules) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	return nil, fmt.Errorf("%s: not indexed", path.Path)
}

func TestSchemaTestValidation(t *testing.T) {
	testCases := []struct {
		dir   string
		file  string
		diags []string
	}{
		{"valid", "basic.tftest.hcl", []string{}},
		{"valid", "child.tftest.hcl", []string{}},
		{"valid", "expect.tftest.hcl", []string{}},
		{"valid", "failing.tftest.hcl", []string{}},
		{"valid", "mocked.tftest.hcl", []string{}},
		{
			// each one is also an error of tofu test 1.12.6 on this file
			"invalid", "bad.tftest.hcl", []string{
				`1: Unexpected block: Blocks of type "test" are not expected here`,
				`6: Unexpected attribute: An attribute named "source" is not expected here`,
				`15: Unexpected attribute: An attribute named "bogus" is not expected here`,
				`17: Required attribute "error_message" not specified: An attribute named "error_message" is required here`,
				`23: Unexpected attribute: An attribute named "nope" is not expected here`,
				`26: Required attribute "source" not specified: An attribute named "source" is required here`,
				`30: Unexpected block: Blocks of type "unknown_block" are not expected here`,
				`33: Too many labels specified for "run": Only 1 label(s) are expected for "run" blocks`,
				`36: Too many blocks specified for "variables": Only 1 block(s) are expected for "variables"`,
			},
		},
		{
			"mocks", "aws.tfmock.hcl", []string{
				`17: Unexpected block: Blocks of type "run" are not expected here`,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.dir+"/"+tc.file, func(t *testing.T) {
			ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})
			gs, err := globalState.NewStateStore()
			if err != nil {
				t.Fatal(err)
			}
			ts, err := state.NewTestStore(gs.ChangeStore)
			if err != nil {
				t.Fatal(err)
			}
			dir, err := filepath.Abs(filepath.Join("testdata", tc.dir))
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.Add(dir); err != nil {
				t.Fatal(err)
			}

			fs := filesystem.NewFilesystem(gs.DocumentStore)
			if err := ParseTestFiles(ctx, fs, ts, dir); err != nil {
				t.Fatal(err)
			}
			if err := SchemaTestValidation(ctx, ts, noModules{}, dir); err != nil {
				t.Fatal(err)
			}

			record, err := ts.TestRecordByPath(dir)
			if err != nil {
				t.Fatal(err)
			}
			if n := record.Diagnostics[globalAst.HCLParsingSource].Count(); n != 0 {
				t.Fatalf("expected no parsing diagnostics, got %d", n)
			}
			got := make([]string, 0)
			for _, diag := range record.Diagnostics[globalAst.SchemaValidationSource][ast.Filename(tc.file)] {
				got = append(got, fmt.Sprintf("%d: %s: %s", diag.Subject.Start.Line, diag.Summary, diag.Detail))
			}
			sort.Slice(got, func(i, j int) bool {
				var a, b int
				fmt.Sscanf(got[i], "%d:", &a)
				fmt.Sscanf(got[j], "%d:", &b)
				return a < b
			})
			if diff := cmp.Diff(tc.diags, got); diff != "" {
				t.Fatalf("unexpected diagnostics: %s", diff)
			}
		})
	}
}

func TestDecodeTestReferences(t *testing.T) {
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	ts, err := state.NewTestStore(gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(filepath.Join("testdata", "valid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Add(dir); err != nil {
		t.Fatal(err)
	}
	fs := filesystem.NewFilesystem(gs.DocumentStore)
	if err := ParseTestFiles(ctx, fs, ts, dir); err != nil {
		t.Fatal(err)
	}
	if err := DecodeTestReferences(ctx, ts, dir); err != nil {
		t.Fatal(err)
	}
	record, err := ts.TestRecordByPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	// the references as written, before they are resolved against modules
	got := make([]string, 0)
	for _, origin := range record.RefOrigins {
		if origin.OriginRange().Filename != "basic.tftest.hcl" {
			continue
		}
		lo, ok := origin.(reference.LocalOrigin)
		if !ok {
			t.Fatalf("expected local origins only, got %#v", origin)
		}
		got = append(got, fmt.Sprintf("%d:%s", lo.Range.Start.Line, lo.Addr))
	}
	want := []string{
		"9:random_pet.name.prefix",
		"9:var.prefix",
		"14:local.label",
		"21:output.pet",
		"26:terraform_data.echo.input",
		"26:random_pet.name.id",
		"35:run.apply_pet.greeting",
		"39:module.child.greeting",
		"39:var.prefix",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected origins: %s", diff)
	}

	// the mock provider is a provider target of its file
	found := false
	for _, target := range record.RefTargets {
		if target.RangePtr != nil && target.RangePtr.Filename == "mocked.tftest.hcl" && target.Addr.String() == "random" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no target for mock_provider \"random\" in %#v", record.RefTargets)
	}
}
