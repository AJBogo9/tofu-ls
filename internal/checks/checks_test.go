// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
	"github.com/opentofu/opentofu-schema/earlydecoder"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/settings"
)

type osFS struct{}

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }

// randomSchema is a provider schema loaded for hashicorp/random 3.6.0.
var randomSchema = &tfschema.ProviderSchema{
	Resources: map[string]*schema.BodySchema{
		"random_pet":    {},
		"random_string": {},
	},
	DataSources: map[string]*schema.BodySchema{
		"random_thing": {},
	},
}

var randomLock = `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.6.0"
}
`

type checksCase struct {
	name string
	// files by path relative to the module directory; .tf, .tofu,
	// .tf.json and .tfvars files are parsed, everything else (lock
	// files, manifests, directories) is only written
	files map[string]string
	// children are the outputs of the called modules, by call name
	children map[string]map[string]bool
	// only runs just this check family
	only string
	want []string
}

func runChecks(t *testing.T, tc checksCase) []string {
	t.Helper()
	dir := t.TempDir()
	mod := &Module{
		Path:         dir,
		Files:        make(map[string]*hcl.File),
		Broken:       make(map[string]bool),
		VarsFiles:    make(map[string]*hcl.File),
		BrokenVars:   make(map[string]bool),
		ChildOutputs: tc.children,
		FS:           osFS{},
		Schema: func(addr tfaddr.Provider, v *version.Version) *tfschema.ProviderSchema {
			if addr.String() == "registry.opentofu.org/hashicorp/random" && v.String() == "3.6.0" {
				return randomSchema
			}
			return nil
		},
	}
	for rel, src := range tc.files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(rel, "/") {
			continue
		}
		var f *hcl.File
		var diags hcl.Diagnostics
		switch {
		case strings.HasSuffix(rel, ".tf.json") || strings.HasSuffix(rel, ".tofu.json"):
			f, diags = hcljson.Parse([]byte(src), rel)
		case strings.HasSuffix(rel, ".tf") || strings.HasSuffix(rel, ".tofu"):
			f, diags = hclsyntax.ParseConfig([]byte(src), rel, hcl.InitialPos)
		case strings.HasSuffix(rel, ".tfvars"):
			f, diags = hclsyntax.ParseConfig([]byte(src), rel, hcl.InitialPos)
			mod.VarsFiles[rel] = f
			mod.BrokenVars[rel] = diags.HasErrors()
			continue
		default:
			continue
		}
		mod.Files[rel] = f
		mod.Broken[rel] = diags.HasErrors()
	}
	meta, _ := earlydecoder.LoadModule(dir, mod.Files)
	mod.Meta = meta

	opts := settings.ValidationOptions{
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
	if tc.only != "" {
		opts = settings.ValidationOptions{EnableEnhancedValidation: true}
		switch tc.only {
		case "duplicates":
			opts.DuplicateDeclarations = true
		case "references":
			opts.UnresolvedReferences = true
		case "types":
			opts.UnknownResourceTypes = true
		case "variables":
			opts.VariableTypes = true
		case "tfvars":
			opts.Tfvars = true
		case "static":
			opts.StaticValues = true
		case "operands":
			opts.OperandTypes = true
		case "installation":
			opts.Installation = true
		case "unused":
			opts.UnusedDataSources = true
		case "interpolation":
			opts.InterpolationOnly = true
		default:
			t.Fatalf("unknown check family %q", tc.only)
		}
	}

	got := make([]string, 0)
	for file, diags := range Run(mod, opts) {
		for _, d := range diags {
			code := ""
			if coded, ok := d.Extra.(ilsp.CodedDiagnostic); ok {
				code = coded.Code
			}
			got = append(got, fmt.Sprintf("%s:%d:%d-%d:%d %s %s", file,
				d.Subject.Start.Line, d.Subject.Start.Column, d.Subject.End.Line, d.Subject.End.Column,
				code, d.Summary))
		}
	}
	sort.Strings(got)
	return got
}

func testChecks(t *testing.T, cases []checksCase) {
	t.Helper()
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			got := runChecks(t, tc)
			want := append([]string{}, tc.want...)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("expected:\n%s\n\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

func TestDuplicates(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "every kind of declaration, the second is flagged",
			only: "duplicates",
			files: map[string]string{
				"a.tf": `variable "v" {}
output "o" { value = 1 }
locals { l = 1 }
module "m" { source = "./m" }
resource "random_pet" "r" {}
data "random_thing" "d" {}
provider "random" {}
provider "random" { alias = "west" }
`,
				"b.tf": `variable "v" {}
output "o" { value = 2 }
locals { l = 2 }
module "m" { source = "./m" }
resource "random_pet" "r" {}
data "random_thing" "d" {}
provider "random" {}
provider "random" { alias = "west" }
provider "random" { alias = "east" }
`,
			},
			want: []string{
				`b.tf:1:10-1:13 duplicate-declaration Duplicate variable declaration`,
				`b.tf:2:8-2:11 duplicate-declaration Duplicate output definition`,
				`b.tf:3:10-3:11 duplicate-declaration Duplicate local value definition`,
				`b.tf:4:8-4:11 duplicate-declaration Duplicate module call`,
				`b.tf:5:10-5:26 duplicate-declaration Duplicate resource "random_pet" configuration`,
				`b.tf:6:6-6:24 duplicate-declaration Duplicate data "random_thing" configuration`,
				`b.tf:7:10-7:18 duplicate-declaration Duplicate provider configuration`,
				`b.tf:8:10-8:18 duplicate-declaration Duplicate provider configuration`,
			},
		},
		{
			name: "duplicates in one file",
			only: "duplicates",
			files: map[string]string{
				"main.tf": `variable "dup" {}
variable "dup" {}
locals {
  a = 1
}
locals {
  a = 2
}
`,
			},
			want: []string{
				`main.tf:2:10-2:15 duplicate-declaration Duplicate variable declaration`,
				`main.tf:7:3-7:4 duplicate-declaration Duplicate local value definition`,
			},
		},
		{
			name: "override files, shadowed .tf files, check-scoped data and dynamic aliases are not duplicates",
			only: "duplicates",
			files: map[string]string{
				"main.tf":          `variable "v" {}` + "\n" + `data "random_thing" "d" {}` + "\n" + `variable "shadow" {}`,
				"override.tf":      `variable "v" { default = 1 }`,
				"x_override.tofu":  `variable "v" { default = 2 }`,
				"shadowed.tf":      `variable "shadow" {}`,
				"shadowed.tofu":    `variable "other" {}`,
				"checks.tf":        `check "c" {` + "\n" + `  data "random_thing" "d" {}` + "\n" + `  assert {` + "\n" + `    condition = true` + "\n" + `    error_message = "x"` + "\n" + `  }` + "\n" + `}`,
				"providers.tf":     `provider "random" { alias = var.a }` + "\n" + `provider "random" { alias = var.a }`,
				"main.tofu.tfvars": ``,
			},
			want: []string{},
		},
		{
			name: "a JSON file declares like any other",
			only: "duplicates",
			files: map[string]string{
				"main.tf":      `variable "v" {}`,
				"more.tf.json": `{"variable": {"v": {}}}`,
			},
			want: []string{
				`more.tf.json:1:15-1:18 duplicate-declaration Duplicate variable declaration`,
			},
		},
		{
			name: "a broken file is left out",
			only: "duplicates",
			files: map[string]string{
				"a.tf": `variable "v" {}`,
				"b.tf": `variable "v" {` + "\n" + `resource "x" {`,
			},
			want: []string{},
		},
	})
}

func TestReferences(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "undeclared resources, data sources, module calls and outputs",
			only: "references",
			files: map[string]string{
				"main.tf": `resource "random_pet" "a" {}
data "random_thing" "d" {}
module "child" {
  source = "./child"
}
module "unknown" {
  source = "./unknown"
}
locals {
  l1 = random_pet.nope.id
  l2 = data.random_thing.nope.x
  l3 = module.nope.x
  l4 = module.child.nope
  l5 = module.child[0].nope
  l6 = module.child.out
  l7 = random_pet.a.id
  l8 = data.random_thing.d.x
  l9 = module.unknown.anything
  l10 = ephemeral.random_pet.nope
}
`,
			},
			children: map[string]map[string]bool{"child": {"out": true}},
			want: []string{
				`main.tf:10:8-10:26 unresolved-reference Reference to undeclared resource`,
				`main.tf:11:8-11:32 unresolved-reference Reference to undeclared resource`,
				`main.tf:12:8-12:21 unresolved-reference Reference to undeclared module`,
				`main.tf:13:8-13:25 unresolved-reference Unsupported attribute`,
				`main.tf:14:8-14:28 unresolved-reference Unsupported attribute`,
				`main.tf:19:9-19:34 unresolved-reference Reference to undeclared resource`,
			},
		},
		{
			name: "each, count and self outside their scope",
			only: "references",
			files: map[string]string{
				"main.tf": `resource "random_pet" "a" {
  prefix = each.key
  length = count.index
  keepers = { x = self.id }
}
locals {
  e = each.value
}
output "c" {
  value = count.index
}
`,
			},
			want: []string{
				`main.tf:2:12-2:20 unavailable-scope Reference to "each" in context without for_each`,
				`main.tf:3:12-3:23 unavailable-scope Reference to "count" in non-counted context`,
				`main.tf:4:19-4:26 unavailable-scope Invalid "self" reference`,
				`main.tf:7:7-7:17 unavailable-scope Reference to "each" in context without for_each`,
				`main.tf:10:11-10:22 unavailable-scope Reference to "count" in non-counted context`,
			},
		},
		{
			name: "valid references are not flagged",
			only: "references",
			files: map[string]string{
				"main.tf": `resource "random_pet" "a" {
  for_each = toset(["x"])
  prefix   = each.key
  keepers  = { k = each.value }
  provider = random.west

  dynamic "setting" {
    for_each = var.settings
    content {
      name  = setting.key
      value = setting.value
    }
  }
  dynamic "other" {
    for_each = var.settings
    iterator = it
    content {
      name = it.value
    }
  }
  provisioner "local-exec" {
    command = self.id
    connection {
      host = self.id
    }
  }
  lifecycle {
    ignore_changes       = [tags, all]
    replace_triggered_by = [random_pet.b]
    postcondition {
      condition     = self.id != ""
      error_message = "x"
    }
  }
}
resource "random_pet" "b" {
  count  = 2
  prefix = count.index
  length = [for s in random_pet.a : s.id]
  keepers = { for k, v in var.m : k => v }
  separator = provider::random::thing(1)
}
resource "random_pet" "c" {
  prefix = "${terraform.workspace}-${path.module}"
  length = random_pet.b[0].id
  keepers = random_pet.b[*].id
  depends_on = [random_pet.a, module.child]
}
moved {
  from = random_pet.old
  to   = random_pet.gone
}
removed {
  from = random_pet.removed
  provisioner "local-exec" {
    when    = destroy
    command = self.id
  }
}
import {
  for_each = var.ids
  to       = random_pet.imported[each.key]
  id       = each.value
  provider = random.west
}
module "child" {
  source    = "./child"
  for_each  = var.m
  name      = each.key
  providers = { random = random.west }
}
provider "random" {
  alias    = "by"
  for_each = var.m
  thing    = each.value
}
check "c" {
  data "random_thing" "scoped" {
    provider = random.west
  }
  assert {
    condition     = data.random_thing.scoped.id != ""
    error_message = "x"
  }
}
output "o" {
  value = [module.child, random_pet.overridden.id, random_pet.json.id, data.random_thing.scoped.x]
}
resource "random_pet" "only_override_for_each" {
  prefix = each.key
}
terraform {
  encryption {
    key_provider "pbkdf2" "k" {
      passphrase = "x"
    }
    method "aes_gcm" "m" {
      keys = key_provider.pbkdf2.k
    }
  }
}
`,
				"override.tf": `resource "random_pet" "overridden" {}
resource "random_pet" "only_override_for_each" {
  for_each = var.m
}
`,
				"extra.tf.json": `{"resource": {"random_pet": {"json": {}}}}`,
			},
			want: []string{},
		},
		{
			name: "var and local are left to the reference validation",
			only: "references",
			files: map[string]string{
				"main.tf": `output "o" { value = [var.nope, local.nope] }`,
			},
			want: []string{},
		},
		{
			name: "a .tofu file shadows the .tf file of the same name",
			only: "references",
			files: map[string]string{
				"main.tf":   `output "o" { value = random_pet.nope.id }`,
				"main.tofu": `output "o" { value = 1 }`,
			},
			want: []string{},
		},
		{
			name: "nothing is reported while a file is broken",
			only: "references",
			files: map[string]string{
				"main.tf":   `output "o" { value = random_pet.nope.id }`,
				"broken.tf": `resource "random_pet" "nope" {`,
			},
			want: []string{},
		},
	})
}

func TestResourceTypes(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "unknown types of a locked provider with a loaded schema",
			only: "types",
			files: map[string]string{
				"main.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
resource "random_pet" "ok" {}
resource "random_pets" "typo" {}
data "random_nope" "d" {}
resource "terraform_data" "builtin" {}
data "terraform_remote_state" "builtin" {}
resource "abbey_demo" "unlocked" {}
resource "x_thing" "aliased" {
  provider = random.west
}
output "o" {
  value = [random_unknown.r.id, random_pet.nope.id, data.random_other.x.id]
}
`,
				".terraform.lock.hcl": randomLock,
			},
			want: []string{
				`main.tf:12:10-12:19 unknown-resource-type Invalid resource type`,
				`main.tf:16:12-16:26 unknown-resource-type Invalid resource type`,
				`main.tf:16:53-16:70 unknown-resource-type Invalid data source`,
				`main.tf:7:10-7:23 unknown-resource-type Invalid resource type`,
				`main.tf:8:6-8:19 unknown-resource-type Invalid data source`,
			},
		},
		{
			name: "no lock file, no check",
			only: "types",
			files: map[string]string{
				"main.tf": `resource "random_nope" "r" {}`,
			},
			want: []string{},
		},
		{
			name: "a locked version without a loaded schema",
			only: "types",
			files: map[string]string{
				"main.tf": `resource "random_nope" "r" {}`,
				".terraform.lock.hcl": `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.7.0"
}
`,
			},
			want: []string{},
		},
	})
}

func TestUnknownTypeSuggestion(t *testing.T) {
	if got := nameSuggestion("random_pets", randomSchema.Resources); got != "random_pet" {
		t.Fatalf("expected random_pet, got %q", got)
	}
	if got := nameSuggestion("aws_instance", randomSchema.Resources); got != "" {
		t.Fatalf("expected no suggestion, got %q", got)
	}
}

func TestVariableTypes(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "invalid type constraints and defaults",
			only: "variables",
			files: map[string]string{
				"main.tf": `variable "kw" {
  type = numbr
}
variable "fn" {
  type = lisst(string)
}
variable "quoted" {
  type = "string"
}
variable "num" {
  type    = number
  default = "not-a-number"
}
variable "list" {
  type    = list(string)
  default = "x"
}
variable "required_attr" {
  type    = object({ a = string })
  default = {}
}
variable "not_nullable" {
  type     = string
  nullable = false
  default  = null
}
`,
			},
			want: []string{
				`main.tf:12:13-12:27 default-type-mismatch Invalid default value for variable`,
				`main.tf:16:13-16:16 default-type-mismatch Invalid default value for variable`,
				`main.tf:20:13-20:15 default-type-mismatch Invalid default value for variable`,
				`main.tf:25:14-25:18 default-type-mismatch Invalid default value for variable`,
				`main.tf:2:10-2:15 invalid-type-constraint Invalid type specification`,
				`main.tf:5:10-5:23 invalid-type-constraint Invalid type specification`,
				`main.tf:8:10-8:18 invalid-type-constraint Invalid quoted type constraints`,
			},
		},
		{
			name: "valid types and defaults",
			only: "variables",
			files: map[string]string{
				"main.tf": `variable "a" {
  type    = number
  default = "5"
}
variable "b" {
  type    = list
  default = ["x", 1]
}
variable "c" {
  type    = map
  default = {}
}
variable "d" {
  type = object({
    a = string
    b = optional(number, 1)
    c = optional(list(object({ x = optional(bool, true) })), [])
  })
  default = { a = "x", c = [{}] }
}
variable "e" {
  type    = any
  default = [1, "x", { a = true }]
}
variable "f" {
  type    = string
  default = null
}
variable "g" {
  type    = map(object({ n = optional(number) }))
  default = { one = {} }
}
variable "h" {
  default = var.a
}
`,
				"override.tf": `variable "a" {
  default = "not-a-number"
}
`,
			},
			want: []string{},
		},
	})
}

func TestTfvars(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "undeclared names, values of the wrong type and non-static values",
			only: "tfvars",
			files: map[string]string{
				"variables.tf": `variable "foo" {}
variable "num" { type = number }
variable "list" { type = list(string) }
variable "obj" {
  type = object({ a = string, b = optional(number, 1) })
}
variable "maybe" { type = number }
`,
				"terraform.tfvars": `foo  = "foo"
bar  = "noot"
num  = "5"
list = "scalar"
obj  = { a = "x" }
foo2 = var.x
foo3 = upper("x")
maybe = null
`,
				"foo.auto.tfvars": `noot = ["a", "b"]
obj  = { b = 2 }
`,
			},
			want: []string{
				`foo.auto.tfvars:1:1-1:5 tfvars-undeclared-variable Value for undeclared variable`,
				`foo.auto.tfvars:2:8-2:17 tfvars-type-mismatch Invalid value for input variable`,
				`terraform.tfvars:2:1-2:4 tfvars-undeclared-variable Value for undeclared variable`,
				`terraform.tfvars:4:8-4:16 tfvars-type-mismatch Invalid value for input variable`,
				`terraform.tfvars:6:1-6:5 tfvars-undeclared-variable Value for undeclared variable`,
				`terraform.tfvars:6:8-6:13 tfvars-not-static Variables not allowed`,
				`terraform.tfvars:7:1-7:5 tfvars-undeclared-variable Value for undeclared variable`,
				`terraform.tfvars:7:8-7:18 tfvars-not-static Function calls not allowed`,
			},
		},
		{
			name: "undeclared names need every declaration",
			only: "tfvars",
			files: map[string]string{
				"variables.tf":     `variable "foo" {}`,
				"broken.tf":        `variable "bar" {`,
				"terraform.tfvars": `bar = 1`,
			},
			want: []string{},
		},
		{
			name: "a type from an override file is not trusted",
			only: "tfvars",
			files: map[string]string{
				"variables.tf":     `variable "n" { type = number }`,
				"override.tf":      `variable "n" { type = string }`,
				"terraform.tfvars": `n = "abc"`,
			},
			want: []string{},
		},
	})
}

func TestTfvarsValueType(t *testing.T) {
	src := `a = "x"
b = [1, 2]
c = { x = true }
d = [1, "x"]
e = []
f = { a = [1] }
`
	f, diags := hclsyntax.ParseConfig([]byte(src), "t.tfvars", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	want := map[string]string{
		"a": "string",
		"b": "list(number)",
		"c": "map(bool)",
		"d": "tuple([number,string])",
		"e": "list(any)",
		"f": "object({a=tuple([number])})",
	}
	for name, attr := range f.Body.(*hclsyntax.Body).Attributes {
		val, _ := attr.Expr.Value(nil)
		if got := typeConstraintFor(val.Type()); got != want[name] {
			t.Errorf("%s: expected %s, got %s", name, want[name], got)
		}
	}
}

func TestStaticValues(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "references and calls where only static values are allowed",
			only: "static",
			files: map[string]string{
				"main.tf": `variable "a" {
  default = "${var.b}-${upper("x")}"
}
resource "random_pet" "p" {
  depends_on = [random_pet.q, upper("x"), "random_pet.q"]
}
output "o" {
  value      = 1
  depends_on = local.deps
}
module "m" {
  source  = "./${random_pet.p.id}"
  version = var.v
}
module "n" {
  source = "./modules/${local.name}"
}
terraform {
  backend "s3" {
    bucket = var.bucket
    key    = data.x.y.key
    assume_role {
      role_arn = module.m.arn
    }
  }
}
`,
			},
			want: []string{
				`main.tf:12:18-12:33 static-reference-required Invalid reference in a static context`,
				`main.tf:21:14-21:26 static-reference-required Invalid reference in a static context`,
				`main.tf:23:18-23:30 static-reference-required Invalid reference in a static context`,
				`main.tf:2:16-2:21 static-reference-required Variables not allowed`,
				`main.tf:2:25-2:31 static-reference-required Function calls not allowed`,
				`main.tf:5:31-5:41 static-reference-required Invalid expression`,
				`main.tf:5:43-5:57 static-reference-required Invalid expression`,
				`main.tf:9:16-9:26 static-reference-required Invalid expression`,
			},
		},
		{
			name: "plain references in depends_on",
			only: "static",
			files: map[string]string{
				"main.tf": `resource "random_pet" "p" {
  depends_on = [random_pet.q[0], module.m, data.x.y, random_pet.r["k"]]
}
`,
			},
			want: []string{},
		},
	})
}

func TestOperands(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "operands which cannot convert",
			only: "operands",
			files: map[string]string{
				"main.tf": `variable "flag" { type = bool }
variable "items" { type = list(string) }
variable "s" { type = string }
variable "n" { type = number }
variable "anything" {}
locals {
  a = "abc" + 1
  b = true + 1
  c = !5
  d = 1 ? 2 : 3
  e = "yes" ? 1 : 2
  f = var.flag + 1
  g = [1] * 2
  h = var.items && true
  i = -var.flag
}
`,
			},
			want: []string{
				`main.tf:10:7-10:8 operand-type-mismatch Incorrect condition type`,
				`main.tf:11:7-11:12 operand-type-mismatch Incorrect condition type`,
				`main.tf:12:7-12:15 operand-type-mismatch Invalid operand`,
				`main.tf:13:7-13:10 operand-type-mismatch Invalid operand`,
				`main.tf:14:7-14:16 operand-type-mismatch Invalid operand`,
				`main.tf:15:8-15:16 operand-type-mismatch Invalid operand`,
				`main.tf:7:7-7:12 operand-type-mismatch Invalid operand`,
				`main.tf:8:7-8:11 operand-type-mismatch Invalid operand`,
				`main.tf:9:8-9:9 operand-type-mismatch Invalid operand`,
			},
		},
		{
			name: "operands which convert or whose type is not known",
			only: "operands",
			files: map[string]string{
				"main.tf": `variable "s" { type = string }
variable "any" { type = any }
variable "l" { type = list(any) }
variable "o" { type = number }
locals {
  a = "5" + 1
  b = var.s + 1
  c = var.any + 1
  d = local.x + 1
  e = null + 1
  f = 1 == "a"
  g = "true" ? 1 : 2
  h = [for x in var.l : x + 1]
  i = length(var.l) > 0 && var.s != ""
  j = var.s ? 1 : 2
  k = !var.s
  l = var.o * 2
  m = var.flag && !var.flag ? var.n + 1 : -var.n
}
variable "flag" { type = bool }
variable "n" { type = number }
`,
				"override.tf": `variable "o" { type = bool }`,
			},
			want: []string{},
		},
	})
}

func TestInstallation(t *testing.T) {
	initialized := map[string]string{
		".terraform/modules/modules.json":                                    `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"vpc","Source":"registry.opentofu.org/terraform-aws-modules/vpc/aws","Version":"5.0.0","Dir":".terraform/modules/vpc"}]}`,
		".terraform/providers/registry.opentofu.org/hashicorp/random/3.6.0/": "",
		".terraform.lock.hcl": randomLock + `provider "registry.opentofu.org/hashicorp/local" {
  version = "2.5.0"
}
`,
		"child/": "",
		"main.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
    local  = { source = "hashicorp/local" }
    null   = { source = "hashicorp/null" }
  }
}
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}
module "other" {
  source = "terraform-aws-modules/eks/aws"
}
module "child" {
  source = "./child"
}
module "missing" {
  source = "./does-not-exist"
}
module "dynamic" {
  source = var.source
}
resource "time_sleep" "t" {}
resource "terraform_data" "d" {}
`,
	}
	uninitialized := map[string]string{
		"main.tf": initialized["main.tf"],
		"child/":  "",
	}

	testChecks(t, []checksCase{
		{
			name:  "an initialized module",
			only:  "installation",
			files: initialized,
			want: []string{
				`main.tf:12:12-12:43 module-not-installed Module not installed`,
				`main.tf:18:12-18:30 module-source-missing Unreadable module directory`,
				`main.tf:23:10-23:22 provider-not-installed Provider not installed`,
				`main.tf:4:25-4:42 provider-not-installed Provider not installed`,
				`main.tf:5:25-5:41 provider-not-installed Provider not installed`,
			},
		},
		{
			name:  "a directory never initialized reports only missing local modules",
			only:  "installation",
			files: uninitialized,
			want: []string{
				`main.tf:18:12-18:30 module-source-missing Unreadable module directory`,
			},
		},
	})
}

func TestUnusedDataSources(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "unused data sources",
			only: "unused",
			files: map[string]string{
				"main.tf": `data "random_thing" "unused" {}
data "random_thing" "used" {}
data "random_thing" "in_depends_on" {}
data "random_thing" "checked" {
  lifecycle {
    postcondition {
      condition     = self.id != ""
      error_message = "x"
    }
  }
}
check "c" {
  data "random_thing" "scoped" {}
  assert {
    condition     = true
    error_message = "x"
  }
}
resource "random_pet" "unused_resource" {
  prefix     = data.random_thing.used[0].id
  depends_on = [data.random_thing.in_depends_on]
}
`,
			},
			want: []string{
				`main.tf:1:6-1:29 unused-data-source Data source "data.random_thing.unused" is declared but not used`,
			},
		},
		{
			name: "a module with JSON files is not searched",
			only: "unused",
			files: map[string]string{
				"main.tf":      `data "random_thing" "unused" {}`,
				"more.tf.json": `{"output": {"o": {"value": "${data.random_thing.unused.id}"}}}`,
			},
			want: []string{},
		},
	})
}

func TestInterpolationOnly(t *testing.T) {
	testChecks(t, []checksCase{
		{
			name: "templates which only wrap an expression",
			only: "interpolation",
			files: map[string]string{
				"main.tf": `locals {
  a = "${var.x}"
  b = "a-${var.x}"
  c = <<EOT
${var.x}
EOT
  d = ["${local.a}"]
}
`,
			},
			want: []string{
				`main.tf:2:7-2:17 interpolation-only Interpolation-only expression`,
				`main.tf:7:8-7:20 interpolation-only Interpolation-only expression`,
			},
		},
	})
}

func TestChecks_invalidTypeKeepsOtherDiagnostics(t *testing.T) {
	// The parity audit saw a module with an invalid type keyword publish
	// nothing at all. The module's other checks still run.
	got := runChecks(t, checksCase{
		files: map[string]string{
			"main.tf": `variable "b" {
  type = numbr
}
output "o" { value = [var.b, random_pet.nope.id] }
`,
		},
	})
	want := []string{
		`main.tf:2:10-2:15 invalid-type-constraint Invalid type specification`,
		`main.tf:4:30-4:48 unresolved-reference Reference to undeclared resource`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}
