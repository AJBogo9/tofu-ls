// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfmod "github.com/opentofu/opentofu-schema/module"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/zclconf/go-cty/cty"
)

const testContextFile = `variables {
  prefix = var.length
}

run "first" {
  assert {
    condition     = random_pet.name.id == var.prefix
    error_message = "x"
  }
}

run "child" {
  module {
    source = "./modules/child"
  }
  variables {
    name = run.first.pet
  }
  assert {
    condition     = output.greeting == var.name
    error_message = "y"
  }
  expect_failures = [var.name]
}

override_resource {
  target = random_pet.name
}
`

type fakeModules struct {
	metas   map[string]*tfmod.Meta
	targets map[string]reference.Targets
}

func (m fakeModules) LocalModuleMeta(modPath string) (*tfmod.Meta, error) {
	if meta, ok := m.metas[modPath]; ok {
		return meta, nil
	}
	return nil, fmt.Errorf("%s: not indexed", modPath)
}

func (m fakeModules) PathContext(path lang.Path) (*decoder.PathContext, error) {
	return m.ReferencePathContext(path)
}

func (m fakeModules) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	if _, ok := m.metas[path.Path]; !ok {
		return nil, fmt.Errorf("%s: not indexed", path.Path)
	}
	return &decoder.PathContext{
		Schema:           &schema.BodySchema{Blocks: map[string]*schema.BlockSchema{}},
		ReferenceTargets: m.targets[path.Path],
	}, nil
}

func addr(steps ...string) lang.Address {
	a := lang.Address{lang.RootStep{Name: steps[0]}}
	for _, s := range steps[1:] {
		a = append(a, lang.AttrStep{Name: s})
	}
	return a
}

// newTestContextRecord returns a record of the test file in root/tests,
// with its origins and targets as the static schema finds them.
func newTestContextRecord(t *testing.T, root string) *state.TestRecord {
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewTestStore(gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "tests")
	if err := store.Add(dir); err != nil {
		t.Fatal(err)
	}
	f, diags := hclsyntax.ParseConfig([]byte(testContextFile), "a.tftest.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if err := store.UpdateParsedFiles(dir, ast.Files{"a.tftest.hcl": f}, nil); err != nil {
		t.Fatal(err)
	}

	d := decoder.NewDecoder(&PathReader{StateReader: store, UseStaticSchema: true})
	pd, err := d.Path(lang.Path{Path: dir, LanguageID: "opentofu-test"})
	if err != nil {
		t.Fatal(err)
	}
	origins, err := pd.CollectReferenceOrigins()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := pd.CollectReferenceTargets()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateReferences(dir, origins, targets, nil); err != nil {
		t.Fatal(err)
	}
	record, err := store.TestRecordByPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func testContextModules(root, child string) fakeModules {
	return fakeModules{
		metas: map[string]*tfmod.Meta{
			root: {
				Path: root,
				Variables: map[string]tfmod.Variable{
					"prefix": {Type: cty.String, Description: "Name prefix"},
					"length": {Type: cty.Number},
				},
				Outputs: map[string]tfmod.Output{"pet": {Description: "The pet"}},
			},
			child: {
				Path:      child,
				Variables: map[string]tfmod.Variable{"name": {Type: cty.String}},
				Outputs:   map[string]tfmod.Output{"greeting": {}},
			},
		},
		targets: map[string]reference.Targets{
			root: {
				{
					Addr:     addr("random_pet", "name"),
					ScopeId:  lang.ScopeId("resource"),
					RangePtr: &hcl.Range{Filename: "main.tf", End: hcl.Pos{Line: 3, Column: 2, Byte: 40}},
				},
				{
					Addr:     addr("random_pet", "name"),
					ScopeId:  lang.ScopeId("resource"),
					Type:     cty.DynamicPseudoType,
					RangePtr: &hcl.Range{Filename: "main.tf", End: hcl.Pos{Line: 3, Column: 2, Byte: 40}},
				},
			},
		},
	}
}

// rangeOf returns the range of the nth (1-based) occurrence of text in
// the test file.
func rangeOf(t *testing.T, text string, nth int) hcl.Range {
	offset := -1
	for i := 0; i < nth; i++ {
		next := strings.Index(testContextFile[offset+1:], text)
		if next < 0 {
			t.Fatalf("occurrence %d of %q not found", nth, text)
		}
		offset += next + 1
	}
	pos := func(byteOffset int) hcl.Pos {
		line := strings.Count(testContextFile[:byteOffset], "\n") + 1
		column := byteOffset - strings.LastIndex(testContextFile[:byteOffset], "\n")
		return hcl.Pos{Line: line, Column: column, Byte: byteOffset}
	}
	return hcl.Range{Filename: "a.tftest.hcl", Start: pos(offset), End: pos(offset + len(text))}
}

func TestTestPathContext_origins(t *testing.T) {
	root := filepath.FromSlash("/work/mod")
	child := filepath.Join(root, "modules", "child")
	record := newTestContextRecord(t, root)
	pathCtx := testPathContext(record, testContextModules(root, child), true)

	testCases := []struct {
		name   string
		text   string
		nth    int
		target string // "" for a local origin, else the module it resolves in
	}{
		{"var in the file's variables", "var.length", 1, root},
		{"key of the file's variables", "prefix", 1, root},
		{"resource in an assert", "random_pet.name.id", 1, root},
		{"var in an assert of a root run", "var.prefix", 1, root},
		{"key of a child run's variables", "name = run", 1, child},
		{"run output in variables", "run.first.pet", 1, ""},
		{"output in a child run", "output.greeting", 1, child},
		{"var in a child run", "var.name", 1, child},
		{"expect_failures of a child run", "var.name", 2, child},
		{"target of a file override", "random_pet.name", 2, root},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rng := rangeOf(t, tc.text, tc.nth)
			if key, _, ok := strings.Cut(tc.text, " = "); ok {
				// the key of an attribute
				rng.End = hcl.Pos{Line: rng.Start.Line, Column: rng.Start.Column + len(key), Byte: rng.Start.Byte + len(key)}
			}
			origins, ok := pathCtx.ReferenceOrigins.AtPos(rng.Filename, rng.Start)
			if !ok {
				t.Fatalf("no origin at %s", rng)
			}
			for _, origin := range origins {
				switch o := origin.(type) {
				case reference.PathOrigin:
					if tc.target == "" {
						t.Fatalf("expected a local origin, got a path origin to %s", o.TargetPath.Path)
					}
					if o.TargetPath.Path != tc.target || o.TargetPath.LanguageID != "opentofu" {
						t.Fatalf("expected a path origin to %s, got %s (%s)", tc.target, o.TargetPath.Path, o.TargetPath.LanguageID)
					}
				case reference.LocalOrigin:
					if tc.target != "" {
						t.Fatalf("expected a path origin to %s, got a local origin", tc.target)
					}
				}
			}
		})
	}

	// output.<name> is a type-less target in its module
	rng := rangeOf(t, "output.greeting", 1)
	origins, _ := pathCtx.ReferenceOrigins.AtPos(rng.Filename, rng.Start)
	po := origins[0].(reference.PathOrigin)
	if len(po.Constraints) != 1 || po.Constraints[0].OfScopeId != tfschema.OutputScope || po.Constraints[0].OfType != cty.NilType {
		t.Fatalf("unexpected output constraints: %#v", po.Constraints)
	}
}

func TestTestPathContext_targetsInScope(t *testing.T) {
	root := filepath.FromSlash("/work/mod")
	child := filepath.Join(root, "modules", "child")
	record := newTestContextRecord(t, root)
	pathCtx := testPathContext(record, testContextModules(root, child), true)

	// the origin's range decides which module's targets are in scope
	at := func(text string, nth int) hcl.Range { return rangeOf(t, text, nth) }
	testCases := []struct {
		name    string
		addr    lang.Address
		rng     hcl.Range
		matches bool
	}{
		{"root var in a root run", addr("var", "prefix"), at("var.prefix", 1), true},
		{"root var in the file's variables", addr("var", "prefix"), at("var.length", 1), true},
		{"root var in a child run's variables", addr("var", "prefix"), at("run.first.pet", 1), true},
		{"root var in a child run's assert", addr("var", "prefix"), at("var.name", 1), false},
		{"child var in a child run's assert", addr("var", "name"), at("var.name", 1), true},
		{"child var in a root run", addr("var", "name"), at("var.prefix", 1), false},
		{"child output in a child run", addr("output", "greeting"), at("output.greeting", 1), true},
		{"root output in a child run", addr("output", "pet"), at("output.greeting", 1), false},
		{"root resource in a root run", addr("random_pet", "name"), at("random_pet.name.id", 1), true},
		{"root resource in a child run", addr("random_pet", "name"), at("output.greeting", 1), false},
		{"root resource in a file override", addr("random_pet", "name"), at("random_pet.name", 2), true},
		{"earlier run's output in variables", addr("run", "first", "pet"), at("run.first.pet", 1), true},
		{"run output in an assert", addr("run", "first", "pet"), at("output.greeting", 1), false},
		{"run output before the run", addr("run", "first", "pet"), at("var.prefix", 1), false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			origin := reference.LocalOrigin{
				Addr:        tc.addr,
				Range:       tc.rng,
				Constraints: reference.OriginConstraints{{OfType: cty.DynamicPseudoType}},
			}
			_, ok := pathCtx.ReferenceTargets.Match(origin)
			if ok != tc.matches {
				t.Fatalf("expected match=%t, got %t", tc.matches, ok)
			}
		})
	}

	// copies of module targets have no range in the test file
	for _, target := range pathCtx.ReferenceTargets {
		if target.ScopeId == tfschema.RunScope {
			if target.RangePtr == nil || target.RangePtr.Filename != "a.tftest.hcl" {
				t.Fatalf("run target without its block's range: %#v", target.RangePtr)
			}
			continue
		}
		if target.RangePtr != nil && target.RangePtr.Filename != "a.tftest.hcl" {
			t.Fatalf("target %s keeps a range in %s", target.Addr, target.RangePtr.Filename)
		}
	}
}

func TestTestPathContext_schema(t *testing.T) {
	root := filepath.FromSlash("/work/mod")
	child := filepath.Join(root, "modules", "child")
	record := newTestContextRecord(t, root)
	pathCtx := testPathContext(record, testContextModules(root, child), true)

	// the file's variables block sets the root's inputs
	if _, ok := pathCtx.Schema.Blocks["variables"].Body.Attributes["prefix"]; !ok {
		t.Fatal("file variables lack the root's variable")
	}
	// each run's variables block sets the inputs of the module it runs
	testCases := []struct {
		run      string
		declared string
	}{
		{"first", "length"},
		{"child", "name"},
	}
	for _, tc := range testCases {
		key := schema.NewSchemaKey(schema.DependencyKeys{Labels: []schema.LabelDependent{{Index: 0, Value: tc.run}}})
		body, ok := pathCtx.Schema.Blocks["run"].DependentBody[key]
		if !ok {
			t.Fatalf("no body for run %q", tc.run)
		}
		if _, ok := body.Blocks["variables"].Body.Attributes[tc.declared]; !ok {
			t.Fatalf("run %q: variables lack %q", tc.run, tc.declared)
		}
	}
	// the static schema is not modified
	if len(staticTestSchema.Blocks["run"].DependentBody) != 0 {
		t.Fatal("the shared static schema was modified")
	}
}

func TestTestPathContext_noModule(t *testing.T) {
	// the root is not indexed: nothing resolves, and nothing crashes
	root := filepath.FromSlash("/work/mod")
	record := newTestContextRecord(t, root)
	pathCtx := testPathContext(record, fakeModules{}, true)

	for _, origin := range pathCtx.ReferenceOrigins {
		if _, ok := origin.(reference.PathOrigin); ok {
			t.Fatalf("path origin without a module: %#v", origin)
		}
	}
	for _, target := range pathCtx.ReferenceTargets {
		if target.ScopeId != tfschema.RunScope && target.ScopeId != lang.ScopeId("provider") {
			t.Fatalf("unexpected target %s", target.Addr)
		}
	}
}
