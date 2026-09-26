// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codelens

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
)

func TestShowZeroReferences(t *testing.T) {
	addr := func(root, name string) lang.Address {
		return lang.Address{lang.RootStep{Name: root}, lang.AttrStep{Name: name}}
	}
	testCases := []struct {
		name     string
		targets  reference.Targets
		expected bool
	}{
		{"variable", reference.Targets{{Addr: addr("var", "stage"), ScopeId: "variable"}}, true},
		{"local", reference.Targets{{Addr: addr("local", "tags"), ScopeId: "local"}}, true},
		{"provider named local", reference.Targets{{Addr: lang.Address{lang.RootStep{Name: "local"}}, ScopeId: "provider"}}, false},
		{"provider alias", reference.Targets{{Addr: addr("local", "secondary"), ScopeId: "provider"}}, false},
		{"output", reference.Targets{{Addr: addr("output", "url"), ScopeId: "output"}}, false},
		{"resource", reference.Targets{{Addr: addr("random_pet", "name"), ScopeId: "resource"}}, false},
		{"module", reference.Targets{{Addr: addr("module", "app"), ScopeId: "module"}}, false},
		{"type-less and typed variable targets", reference.Targets{
			{Addr: addr("var", "stage")},
			{Addr: addr("var", "stage"), ScopeId: "variable"},
		}, true},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			if got := showZeroReferences(tc.targets); got != tc.expected {
				t.Fatalf("expected %t, got %t", tc.expected, got)
			}
		})
	}
}

type countingPathReader struct {
	paths    []lang.Path
	pathsN   int
	contextN map[lang.Path]int
}

func (r *countingPathReader) Paths(ctx context.Context) []lang.Path {
	r.pathsN++
	return r.paths
}

func (r *countingPathReader) PathContext(path lang.Path) (*decoder.PathContext, error) {
	r.contextN[path]++
	return &decoder.PathContext{}, nil
}

func TestCachedPathReader(t *testing.T) {
	a := lang.Path{Path: "/a", LanguageID: "opentofu"}
	b := lang.Path{Path: "/b", LanguageID: "opentofu"}
	inner := &countingPathReader{paths: []lang.Path{a, b}, contextN: map[lang.Path]int{}}
	r := newCachedPathReader(inner)

	// what OriginsTargeting does once per target: every path's context
	for target := 0; target < 50; target++ {
		for _, p := range r.Paths(context.Background()) {
			if _, err := r.PathContext(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if inner.pathsN != 1 {
		t.Errorf("paths listed %d times, want 1", inner.pathsN)
	}
	for _, p := range []lang.Path{a, b} {
		if inner.contextN[p] != 1 {
			t.Errorf("context of %s built %d times, want 1", p.Path, inner.contextN[p])
		}
	}
}

func TestOriginKindAndSuffix(t *testing.T) {
	modPath := lang.Path{Path: "/mod", LanguageID: "opentofu"}
	varsPath := lang.Path{Path: "/mod", LanguageID: "opentofu-vars"}
	parentPath := lang.Path{Path: "/", LanguageID: "opentofu"}
	declRange := hcl.Range{Filename: "variables.tf", Start: hcl.Pos{Line: 1, Column: 1, Byte: 0}, End: hcl.Pos{Line: 6, Column: 2, Byte: 120}}
	target := reference.Target{
		Addr:     lang.Address{lang.RootStep{Name: "var"}, lang.AttrStep{Name: "x"}},
		RangePtr: &declRange,
	}
	inValidation := hcl.Range{Filename: "variables.tf", Start: hcl.Pos{Line: 3, Column: 20, Byte: 60}, End: hcl.Pos{Line: 3, Column: 25, Byte: 65}}
	elsewhere := hcl.Range{Filename: "main.tf", Start: hcl.Pos{Line: 2, Column: 3, Byte: 10}, End: hcl.Pos{Line: 2, Column: 8, Byte: 15}}

	testCases := []struct {
		name    string
		origins []decoder.PathOrigin
		want    string
	}{
		{"a use", []decoder.PathOrigin{{Path: modPath, Origin: reference.LocalOrigin{Range: elsewhere}}}, ""},
		{"tfvars only", []decoder.PathOrigin{{Path: varsPath, Origin: reference.PathOrigin{Range: elsewhere}}}, " (tfvars only)"},
		{"own validation only", []decoder.PathOrigin{{Path: modPath, Origin: reference.LocalOrigin{Range: inValidation}}}, " (own validation only)"},
		{"module argument and validation", []decoder.PathOrigin{
			{Path: parentPath, Origin: reference.PathOrigin{Range: elsewhere}},
			{Path: modPath, Origin: reference.LocalOrigin{Range: inValidation}},
		}, " (module arguments, own validation only)"},
		{"a use among others", []decoder.PathOrigin{
			{Path: varsPath, Origin: reference.PathOrigin{Range: elsewhere}},
			{Path: modPath, Origin: reference.LocalOrigin{Range: elsewhere}},
		}, ""},
	}
	output := reference.Target{
		Addr:     lang.Address{lang.RootStep{Name: "output"}, lang.AttrStep{Name: "url"}},
		RangePtr: &declRange,
	}
	if got := originKind(decoder.PathOrigin{Path: parentPath, Origin: reference.PathOrigin{Range: elsewhere}}, output, modPath); got != originUse {
		t.Errorf("module.x.url is a use of output url, got %q", got)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kinds := make(map[originKey]string)
			for _, po := range tc.origins {
				kinds[originKey{path: po.Path, rng: po.Origin.OriginRange()}] = originKind(po, target, modPath)
			}
			if got := nonUseSuffix(kinds); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
