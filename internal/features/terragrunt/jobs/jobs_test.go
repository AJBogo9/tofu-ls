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
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// decodeDir parses, decodes the references of and validates the
// Terragrunt files of dir, which is relative to testdata unless it is
// absolute.
func decodeDir(t *testing.T, dir string) (*state.TerragruntStore, *state.TerragruntRecord) {
	t.Helper()
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewTerragruntStore(gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) {
		dir, err = filepath.Abs(filepath.Join("testdata", dir))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Add(dir); err != nil {
		t.Fatal(err)
	}
	fs := filesystem.NewFilesystem(gs.DocumentStore)
	if err := ParseTerragruntFiles(ctx, fs, store, dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := DecodeTerragruntReferences(ctx, store, dir); err != nil {
		t.Fatal(err)
	}
	if err := SchemaTerragruntValidation(ctx, store, dir); err != nil {
		t.Fatal(err)
	}
	record, err := store.TerragruntRecordByPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, record
}

func diagStrings(diags hcl.Diagnostics) []string {
	got := make([]string, 0)
	for _, diag := range diags {
		severity := "error"
		if diag.Severity == hcl.DiagWarning {
			severity = "warning"
		}
		got = append(got, fmt.Sprintf("%d: %s: %s: %s", diag.Subject.Start.Line, severity, diag.Summary, diag.Detail))
	}
	sort.Slice(got, func(i, j int) bool {
		var a, b int
		fmt.Sscanf(got[i], "%d:", &a)
		fmt.Sscanf(got[j], "%d:", &b)
		return a < b
	})
	return got
}

func TestSchemaTerragruntValidation(t *testing.T) {
	testCases := []struct {
		dir      string
		file     string
		language string
		diags    []string
	}{
		// every block and attribute of the reference
		{"valid", "terragrunt.hcl", "terragrunt", []string{}},
		// the attribute forms of remote_state and generate
		{"valid-root", "root.hcl", "terragrunt", []string{}},
		{"valid-stack", "terragrunt.stack.hcl", "terragrunt-stack", []string{}},
		{
			// warnings, never errors: Terragrunt may be newer than the schema
			"invalid", "terragrunt.hcl", "terragrunt", []string{
				`3: warning: Unknown Terragrunt attribute: Terragrunt 1.1.6 has no attribute "sources" here. It may be a typo, or new in a later Terragrunt version.`,
				`6: warning: Unknown Terragrunt attribute: Terragrunt 1.1.6 has no attribute "skip" here. It may be a typo, or new in a later Terragrunt version.`,
				`10: warning: Unknown Terragrunt attribute: Terragrunt 1.1.6 has no attribute "bucket" here. It may be a typo, or new in a later Terragrunt version.`,
				`15: warning: Unknown Terragrunt attribute: Terragrunt 1.1.6 has no attribute "outputs" here. It may be a typo, or new in a later Terragrunt version.`,
				`22: warning: Unknown Terragrunt block: Terragrunt 1.1.6 has no block "terragrunt_cloud" here. It may be a typo, or new in a later Terragrunt version.`,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.dir+"/"+tc.file, func(t *testing.T) {
			_, record := decodeDir(t, tc.dir)
			if n := record.Diagnostics[globalAst.HCLParsingSource].Count(); n != 0 {
				t.Fatalf("expected no parsing diagnostics, got %d: %v", n, record.Diagnostics[globalAst.HCLParsingSource])
			}
			if got := record.Languages[ast.Filename(tc.file)]; got != tc.language {
				t.Errorf("language: got %q, want %q", got, tc.language)
			}
			got := diagStrings(record.Diagnostics[globalAst.SchemaValidationSource][ast.Filename(tc.file)])
			if diff := cmp.Diff(tc.diags, got); diff != "" {
				t.Fatalf("unexpected diagnostics: %s", diff)
			}
		})
	}
}

func TestDecodeTerragruntReferences(t *testing.T) {
	_, record := decodeDir(t, "valid")
	origins := make([]string, 0)
	for _, origin := range record.RefOrigins {
		lo, ok := origin.(reference.LocalOrigin)
		if !ok {
			t.Fatalf("expected local origins only, got %#v", origin)
		}
		origins = append(origins, fmt.Sprintf("%d:%s", lo.Range.Start.Line, lo.Addr))
	}
	sort.Strings(origins)
	wantOrigins := []string{
		"107:feature.fast.value",
		"141:local.tags",
		"142:dependency.vpc.outputs.vpc_id",
		// s.outputs.id is the for expression's own iterator
		"143:dependency.subnets",
		"144:include.root.locals.account",
		"51:local.region",
		"97:local.region",
	}
	sort.Strings(wantOrigins)
	if diff := cmp.Diff(wantOrigins, origins); diff != "" {
		t.Errorf("origins: %s", diff)
	}

	// include targets depend on the included file, so the path context
	// adds them, not the job
	wantTargets := []string{
		"102:feature.fast",
		// a local is a target as a typed value and as a reference, as
		// in OpenTofu's locals block
		"10:local.tags",
		"10:local.tags",
		"66:dependency.vpc",
		"75:dependency.subnets",
		"9:local.region",
		"9:local.region",
	}
	if diff := cmp.Diff(wantTargets, targetStrings(record.RefTargets)); diff != "" {
		t.Errorf("targets: %s", diff)
	}

	_, stack := decodeDir(t, "valid-stack")
	wantTargets = []string{"29:unit.db", "34:unit.shard", "43:stack.services", "6:local.stage", "6:local.stage", "9:unit.vpc"}
	sort.Strings(wantTargets)
	if diff := cmp.Diff(wantTargets, targetStrings(stack.RefTargets)); diff != "" {
		t.Errorf("stack targets: %s", diff)
	}
}

func targetStrings(targets reference.Targets) []string {
	got := make([]string, 0)
	for _, target := range targets {
		got = append(got, fmt.Sprintf("%d:%s", target.RangePtr.Start.Line, target.Addr))
	}
	sort.Strings(got)
	return got
}
