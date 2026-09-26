// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package validations

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/zclconf/go-cty/cty"
)

func TestUnreferencedOrigins(t *testing.T) {
	tests := []struct {
		name    string
		origins reference.Origins
		want    lang.DiagnosticsMap
	}{
		{
			name: "undeclared variable",
			origins: reference.Origins{
				reference.LocalOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{},
						End:      hcl.Pos{},
					},
					Addr: lang.Address{
						lang.RootStep{Name: "var"},
						lang.AttrStep{Name: "foo"},
					},
				},
			},
			want: lang.DiagnosticsMap{
				"test.tf": hcl.Diagnostics{
					&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "No declaration found for \"var.foo\"",
						Extra: ilsp.CodedDiagnostic{
							Code: ilsp.CodeUnresolvedReference,
							Data: map[string]interface{}{"address": "var.foo"},
						},
						Subject: &hcl.Range{
							Filename: "test.tf",
							Start:    hcl.Pos{},
							End:      hcl.Pos{},
						},
					},
				},
			},
		},
		{
			name: "undeclared local value",
			origins: reference.Origins{
				reference.LocalOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{},
						End:      hcl.Pos{},
					},
					Addr: lang.Address{
						lang.RootStep{Name: "local"},
						lang.AttrStep{Name: "foo"},
					},
				},
			},
			want: lang.DiagnosticsMap{
				"test.tf": hcl.Diagnostics{
					&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "No declaration found for \"local.foo\"",
						Extra: ilsp.CodedDiagnostic{
							Code: ilsp.CodeUnresolvedReference,
							Data: map[string]interface{}{"address": "local.foo"},
						},
						Subject: &hcl.Range{
							Filename: "test.tf",
							Start:    hcl.Pos{},
							End:      hcl.Pos{},
						},
					},
				},
			},
		},
		{
			name: "unsupported variable of complex type",
			origins: reference.Origins{
				reference.LocalOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{},
						End:      hcl.Pos{},
					},
					Addr: lang.Address{
						lang.RootStep{Name: "var"},
						lang.AttrStep{Name: "obj"},
						lang.AttrStep{Name: "field"},
					},
				},
			},
			want: lang.DiagnosticsMap{},
		},
		{
			name: "unsupported path origins (module input)",
			origins: reference.Origins{
				reference.PathOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{},
						End:      hcl.Pos{},
					},
					TargetAddr: lang.Address{
						lang.RootStep{Name: "var"},
						lang.AttrStep{Name: "foo"},
					},
					TargetPath: lang.Path{
						Path:       "./submodule",
						LanguageID: "opentofu",
					},
					Constraints: reference.OriginConstraints{},
				},
			},
			want: lang.DiagnosticsMap{},
		},
		{
			name: "many undeclared variables",
			origins: reference.Origins{
				reference.LocalOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{Line: 1, Column: 1, Byte: 0},
						End:      hcl.Pos{Line: 1, Column: 10, Byte: 10},
					},
					Addr: lang.Address{
						lang.RootStep{Name: "var"},
						lang.AttrStep{Name: "foo"},
					},
				},
				reference.LocalOrigin{
					Range: hcl.Range{
						Filename: "test.tf",
						Start:    hcl.Pos{Line: 2, Column: 1, Byte: 0},
						End:      hcl.Pos{Line: 2, Column: 10, Byte: 10},
					},
					Addr: lang.Address{
						lang.RootStep{Name: "var"},
						lang.AttrStep{Name: "wakka"},
					},
				},
			},
			want: lang.DiagnosticsMap{
				"test.tf": hcl.Diagnostics{
					&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "No declaration found for \"var.foo\"",
						Extra: ilsp.CodedDiagnostic{
							Code: ilsp.CodeUnresolvedReference,
							Data: map[string]interface{}{"address": "var.foo"},
						},
						Subject: &hcl.Range{
							Filename: "test.tf",
							Start:    hcl.Pos{Line: 1, Column: 1, Byte: 0},
							End:      hcl.Pos{Line: 1, Column: 10, Byte: 10},
						},
					},
					&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "No declaration found for \"var.wakka\"",
						Extra: ilsp.CodedDiagnostic{
							Code: ilsp.CodeUnresolvedReference,
							Data: map[string]interface{}{"address": "var.wakka"},
						},
						Subject: &hcl.Range{
							Filename: "test.tf",
							Start:    hcl.Pos{Line: 2, Column: 1, Byte: 0},
							End:      hcl.Pos{Line: 2, Column: 10, Byte: 10},
						},
					},
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%2d-%s", i, tt.name), func(t *testing.T) {
			ctx := context.Background()

			pathCtx := &decoder.PathContext{
				ReferenceOrigins: tt.origins,
			}

			diags := UnreferencedOrigins(ctx, pathCtx)
			if diff := cmp.Diff(tt.want["test.tf"], diags["test.tf"]); diff != "" {
				t.Fatalf("unexpected diagnostics: %s", diff)
			}
		})
	}
}

func TestUnreferencedOrigins_declaredWithOtherType(t *testing.T) {
	// "${var.list}" returns the list unchanged, but its origin expects a
	// string: the variable is declared, so nothing is reported
	pathCtx := &decoder.PathContext{
		ReferenceOrigins: reference.Origins{
			reference.LocalOrigin{
				Range: hcl.Range{Filename: "test.tf"},
				Addr: lang.Address{
					lang.RootStep{Name: "var"},
					lang.AttrStep{Name: "list"},
				},
				Constraints: reference.OriginConstraints{{OfType: cty.String}},
			},
		},
		ReferenceTargets: reference.Targets{
			{
				Addr: lang.Address{
					lang.RootStep{Name: "var"},
					lang.AttrStep{Name: "list"},
				},
				Type: cty.List(cty.Bool),
			},
		},
	}

	if _, ok := pathCtx.ReferenceTargets.Match(pathCtx.ReferenceOrigins[0].(reference.LocalOrigin)); ok {
		t.Fatal("expected the origin's type constraint not to match the target")
	}
	diags := UnreferencedOrigins(context.Background(), pathCtx)
	if len(diags["test.tf"]) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags["test.tf"])
	}
}
