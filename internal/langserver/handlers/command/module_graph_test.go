// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func parseGraphFiles(t *testing.T, src map[string]string) map[string]*hcl.File {
	t.Helper()
	files := make(map[string]*hcl.File, len(src))
	for name, text := range src {
		f, diags := hclsyntax.ParseConfig([]byte(text), name, hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("parsing %s: %s", name, diags)
		}
		files[name] = f
	}
	return files
}

// graphNodeStrings renders nodes as "kind id" plus the fields that are set.
func graphNodeStrings(nodes []moduleGraphNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		s := fmt.Sprintf("%s %s @%s:%d", n.Kind, n.ID, filepath.Base(n.URI), n.NameRange.Start.Line+1)
		if n.Type != "" {
			s += " type=" + n.Type
		}
		if n.Description != "" {
			s += fmt.Sprintf(" description=%q", n.Description)
		}
		if n.Detail != "" {
			s += fmt.Sprintf(" detail=%q", n.Detail)
		}
		if n.Sensitive {
			s += " sensitive"
		}
		if n.Repeat != "" {
			s += " repeat=" + n.Repeat
		}
		out = append(out, s)
	}
	return out
}

// graphEdgeStrings renders edges as "from -> to" and each reference as
// "attribute: text @line:col".
func graphEdgeStrings(edges []moduleGraphEdge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		refs := make([]string, 0, len(e.Refs))
		for _, r := range e.Refs {
			refs = append(refs, fmt.Sprintf("%s: %s @%d:%d", r.Attribute, r.Text, r.Range.Start.Line+1, r.Range.Start.Character+1))
		}
		out = append(out, fmt.Sprintf("%s -> %s [%s]", e.From, e.To, strings.Join(refs, ", ")))
	}
	return out
}

func Test_buildModuleGraph(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantNodes []string
		wantEdges []string
	}{
		{
			name:      "empty module",
			files:     map[string]string{"main.tf": ""},
			wantNodes: []string{},
			wantEdges: []string{},
		},
		{
			name: "variable through local into resource and output",
			files: map[string]string{
				"main.tf": `variable "stage" {
  type        = string
  description = "Deployment stage"
}

locals {
  prefix = "app-${var.stage}"
}

resource "random_pet" "name" {
  prefix = local.prefix
}

output "pet" {
  value       = random_pet.name.id
  description = "The pet name"
  sensitive   = true
}
`,
			},
			wantNodes: []string{
				`variable var.stage @main.tf:1 description="Deployment stage" detail="string"`,
				`local local.prefix @main.tf:7 detail="\"app-${var.stage}\""`,
				`resource random_pet.name @main.tf:10 type=random_pet`,
				`output output.pet @main.tf:14 description="The pet name" sensitive`,
			},
			wantEdges: []string{
				"local.prefix -> random_pet.name [prefix: local.prefix @11:12]",
				"random_pet.name -> output.pet [value: random_pet.name.id @15:17]",
				"var.stage -> local.prefix [: var.stage @7:19]",
			},
		},
		{
			name: "references in nested blocks, for expressions and depends_on",
			files: map[string]string{
				"main.tf": `variable "zones" {}
variable "names" {}

data "local_file" "motd" {
  filename = "motd.txt"
}

resource "null_resource" "worker" {
  count = length(var.zones)
  triggers = {
    zones = join(",", [for z in var.zones : upper(z)])
    motd  = data.local_file.motd.content
  }
  provisioner "local-exec" {
    command = "echo ${count.index} ${self.id} ${path.module}"
  }
}

resource "terraform_data" "after" {
  for_each   = toset(var.names)
  input      = each.value
  depends_on = [null_resource.worker]
  lifecycle {
    replace_triggered_by = [null_resource.worker[0].id]
  }
}
`,
			},
			wantNodes: []string{
				"variable var.zones @main.tf:1",
				"variable var.names @main.tf:2",
				"data data.local_file.motd @main.tf:4 type=local_file",
				"resource null_resource.worker @main.tf:8 type=null_resource repeat=count",
				"resource terraform_data.after @main.tf:19 type=terraform_data repeat=for_each",
			},
			wantEdges: []string{
				"data.local_file.motd -> null_resource.worker [triggers: data.local_file.motd.content @12:13]",
				"null_resource.worker -> terraform_data.after [depends_on: null_resource.worker @22:17, lifecycle.replace_triggered_by: null_resource.worker[0].id @24:29]",
				"var.names -> terraform_data.after [for_each: var.names @20:22]",
				"var.zones -> null_resource.worker [count: var.zones @9:18, triggers: var.zones @11:33]",
			},
		},
		{
			name: "provider references resolve to provider configurations, not locals",
			files: map[string]string{
				"providers.tf": `provider "local" {}

provider "local" {
  alias = "secondary"
}

provider "google" {
  project = var.project
}
`,
				"main.tf": `variable "project" {}

locals {
  secondary = "not a provider"
}

resource "local_file" "a" {
  provider = local.secondary
  content  = local.secondary
}

module "child" {
  source = "./child"
  providers = {
    local            = local.secondary
    google.secondary = google
  }
}
`,
			},
			wantNodes: []string{
				"variable var.project @main.tf:1",
				"local local.secondary @main.tf:4 detail=\"\\\"not a provider\\\"\"",
				"resource local_file.a @main.tf:7 type=local_file",
				`module module.child @main.tf:12 detail="./child"`,
				"provider provider.local @providers.tf:1 type=local",
				"provider provider.local.secondary @providers.tf:3 type=local",
				"provider provider.google @providers.tf:7 type=google",
			},
			wantEdges: []string{
				"local.secondary -> local_file.a [content: local.secondary @9:14]",
				"provider.google -> module.child [providers: google @16:24]",
				"provider.local.secondary -> local_file.a [provider: local.secondary @8:14]",
				"provider.local.secondary -> module.child [providers: local.secondary @15:24]",
				"var.project -> provider.google [project: var.project @8:13]",
			},
		},
		{
			name: "unknown references, iterators and self references make no edges",
			files: map[string]string{
				"main.tf": `locals {
  a = var.undeclared
  b = local.b
  c = terraform.workspace
}

resource "google_compute_firewall" "fw" {
  dynamic "allow" {
    for_each = local.a
    content {
      ports = allow.value
    }
  }
}
`,
			},
			wantNodes: []string{
				"local local.a @main.tf:2 detail=\"var.undeclared\"",
				"local local.b @main.tf:3 detail=\"local.b\"",
				"local local.c @main.tf:4 detail=\"terraform.workspace\"",
				"resource google_compute_firewall.fw @main.tf:7 type=google_compute_firewall",
			},
			wantEdges: []string{
				"local.a -> google_compute_firewall.fw [allow.for_each: local.a @9:16]",
			},
		},
		{
			name: "module outputs and a duplicate declaration",
			files: map[string]string{
				"a.tf": `module "app" {
  source = "./modules/app"
  name   = var.name
}

output "url" {
  value = module.app.url
}
`,
				"b.tf": `variable "name" {
  type = object({
    first = string
    last  = string
  })
}

variable "name" {}
`,
			},
			wantNodes: []string{
				`module module.app @a.tf:1 detail="./modules/app"`,
				"output output.url @a.tf:6",
				`variable var.name @b.tf:1 detail="object({ first = string last = string })"`,
			},
			wantEdges: []string{
				"module.app -> output.url [value: module.app.url @7:11]",
				"var.name -> module.app [name: var.name @3:12]",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modPath := t.TempDir()
			got := buildModuleGraph(modPath, parseGraphFiles(t, tt.files), nil)
			if got.ModuleURI != uri.FromPath(modPath) {
				t.Errorf("module URI: got %q, want %q", got.ModuleURI, uri.FromPath(modPath))
			}
			if diff := cmp.Diff(tt.wantNodes, graphNodeStrings(got.Nodes)); diff != "" {
				t.Errorf("nodes mismatch: %s", diff)
			}
			if diff := cmp.Diff(tt.wantEdges, graphEdgeStrings(got.Edges)); diff != "" {
				t.Errorf("edges mismatch: %s", diff)
			}
		})
	}
}

func Test_buildModuleGraph_childBoundary(t *testing.T) {
	modPath := t.TempDir()
	files := parseGraphFiles(t, map[string]string{
		"main.tf": `module "app" {
  source     = "./modules/app"
  name       = "x"
  missing    = 1
  depends_on = []
}

module "remote" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "1.0.0"
}

output "url" {
  value = module.app.url
}

output "gone" {
  value = module.app.gone
}

output "indexed" {
  value = module.app[0].first
}
`,
	})
	childDir := filepath.Join(modPath, "modules", "app")
	childFiles := parseGraphFiles(t, map[string]string{
		"variables.tf": `variable "name" {}
`,
		"outputs.tf": `output "url" {
  value = var.name
}
`,
	})
	resolve := func(name, source string) (string, map[string]*hcl.File) {
		if name == "app" && source == "./modules/app" {
			return childDir, childFiles
		}
		return "", nil
	}

	got := buildModuleGraph(modPath, files, resolve)

	children := make(map[string]*moduleGraphChild)
	for _, n := range got.Nodes {
		if n.Kind == "module" {
			children[n.ID] = n.Child
		}
	}
	if children["module.remote"] != nil {
		t.Errorf("expected no child boundary for an unresolved source, got %#v", children["module.remote"])
	}

	varRange := lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 15}}
	outRange := lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 12}}
	want := &moduleGraphChild{
		URI: uri.FromPath(childDir),
		Inputs: []moduleGraphPort{
			{Name: "name", URI: uri.FromPath(filepath.Join(childDir, "variables.tf")), Range: &varRange},
			{Name: "missing"},
		},
		Outputs: []moduleGraphPort{
			{Name: "first"},
			{Name: "gone"},
			{Name: "url", URI: uri.FromPath(filepath.Join(childDir, "outputs.tf")), Range: &outRange},
		},
	}
	if diff := cmp.Diff(want, children["module.app"]); diff != "" {
		t.Errorf("child boundary mismatch: %s", diff)
	}
}

func Test_graphTargetID(t *testing.T) {
	tests := []struct {
		expr        string
		providerRef bool
		want        string
		wantOk      bool
	}{
		{"var.region", false, "var.region", true},
		{"local.name", false, "local.name", true},
		{"module.vpc.id", false, "module.vpc", true},
		{"data.aws_ami.ubuntu.id", false, "data.aws_ami.ubuntu", true},
		{"aws_instance.web[0].id", false, "aws_instance.web", true},
		{"aws_instance.web", false, "aws_instance.web", true},
		{"aws", true, "provider.aws", true},
		{"aws.west", true, "provider.aws.west", true},
		{"local.secondary", true, "provider.local.secondary", true},
		{"data.aws_ami", false, "", false},
		{"var", false, "", false},
		{"each.value", false, "", false},
		{"count.index", false, "", false},
		{"self.id", false, "", false},
		{"path.module", false, "", false},
		{"terraform.workspace", false, "", false},
		{"var[0]", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			traversal, diags := hclsyntax.ParseTraversalAbs([]byte(tt.expr), "test.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			got, ok := graphTargetID(traversal, tt.providerRef)
			if got != tt.want || ok != tt.wantOk {
				t.Errorf("graphTargetID(%s, %t) = %q, %t; want %q, %t", tt.expr, tt.providerRef, got, ok, tt.want, tt.wantOk)
			}
		})
	}
}
