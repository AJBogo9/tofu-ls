// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
)

// hintStrings renders hints as "line ref label" for comparisons.
func hintStrings(hints []InlayHint) []string {
	out := make([]string, 0, len(hints))
	for _, h := range hints {
		out = append(out, fmt.Sprintf("%d %s %s", h.Pos.Line, h.Ref, h.Label))
	}
	return out
}

const inlayPolicyConfig = `variable "on" {
  default = false
}
variable "n" {
  default = 2
}
variable "services" {
  default = {
    web    = 443
    worker = 9000
  }
}
variable "stage" {
  default = "prod"
}
variable "empty" {
  default = []
}
variable "nothing" {
  default = null
}
variable "key" {
  default = "a"
}
variable "secret" {
  default   = "hunter2"
  sensitive = true
}
variable "yes" {
  default = true
}
locals {
  name  = format("%s-%s", "app", var.stage)
  plain = "literal"
  m     = { a = "x" }
  where = "${path.module}/files"
}
resource "terraform_data" "legacy" {
  count = var.on ? 1 : 0
}
resource "terraform_data" "svc" {
  for_each = var.services
  input = {
    name  = "${local.name}-${each.key}"
    port  = each.value
    label = var.on ? "on" : null
    host  = "${var.stage}.example"
  }
}
resource "terraform_data" "late" {
  count = var.n + length(terraform_data.legacy)
}
resource "terraform_data" "misc" {
  input = [var.empty, var.nothing, local.m[var.key], var.stage, var.n, var.on, local.plain]
}
resource "terraform_data" "hidden" {
  input = var.yes ? var.secret : "x"
}
resource "terraform_data" "doc" {
  input = <<-EOT
    stage ${var.stage}
  EOT
}
resource "terraform_data" "files" {
  input = "${path.module}/x"
}
`

func TestInlayHintsWith_policy(t *testing.T) {
	root := writeTree(t, map[string]string{"main.tf": inlayPolicyConfig})
	v, w := ValueHintPrefix, WholeHintPrefix
	testCases := []struct {
		name   string
		policy InlayPolicy
		want   []string
	}{
		{
			"all: a hint after every known reference",
			InlayAll,
			[]string{
				`33 var.stage ` + v + `"prod"`,
				`39 var.on ` + v + `false`,
				`42 var.services ` + v + `{ web = 443, worker = 9000 }`,
				`44 local.name ` + v + `"app-prod"`,
				`46 var.on ` + v + `false`,
				`47 var.stage ` + v + `"prod"`,
				`51 var.n ` + v + `2`,
				`54 var.empty ` + v + `[]`,
				`54 var.nothing ` + v + `null`,
				`54 local.m ` + v + `{ a = "x" }`,
				`54 var.key ` + v + `"a"`,
				`54 var.stage ` + v + `"prod"`,
				`54 var.n ` + v + `2`,
				`54 var.on ` + v + `false`,
				`54 local.plain ` + v + `"literal"`,
				`57 var.yes ` + v + `true`,
				`61 var.stage ` + v + `"prod"`,
			},
		},
		{
			"informative",
			InlayInformative,
			[]string{
				// a local's own value, instead of hints inside it
				`33 name ` + w + `"app-prod"`,
				// count: the number of instances
				`39 count ` + w + `0 instances`,
				// for_each: the instance keys
				`42 for_each ` + w + `web, worker`,
				// a template with interpolations: its value, and nothing
				// inside (local.name-each.key is not known)
				// a conditional keeps a null result: it says which branch won
				`46  ` + w + `null`,
				`47  ` + w + `"prod.example"`,
				// count not known statically: hints inside it instead
				`51 var.n ` + v + `2`,
				// no hints for null or empty values, before an index, or
				// beyond three on a line
				`54 var.key ` + v + `"a"`,
				`54 var.stage ` + v + `"prod"`,
				`54 var.n ` + v + `2`,
				// a conditional with a sensitive value gets no hint on the
				// whole, and its references keep their own hints
				`57 var.yes ` + v + `true`,
				// nothing inside a heredoc, and no hint on a template of
				// path.module alone
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := treeEvaluator(t, root, nil)
			got := hintStrings(ev.InlayHintsWith("main.tf", hcl.Range{}, InlayOptions{Policy: tc.policy}))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected hints (-want +got):\n%s", diff)
			}
			for _, g := range got {
				if strings.Contains(g, "hunter2") {
					t.Errorf("sensitive value in a hint: %q", g)
				}
			}
		})
	}
}

func TestInlayHintsWith_instanceKeys(t *testing.T) {
	testCases := []struct {
		name   string
		config string
		want   []string
	}{
		{
			"a set of strings lists its members, quoted where needed",
			`variable "zones" {
  default = ["north-a", "10.0.0.0/24"]
}
resource "terraform_data" "z" {
  for_each = toset(var.zones)
}
`,
			[]string{`5 for_each ` + WholeHintPrefix + `"10.0.0.0/24", north-a`},
		},
		{
			"an empty map has no instances",
			`variable "m" {
  default = {}
}
module "m" {
  source   = "./x"
  for_each = var.m
}
`,
			[]string{`6 for_each ` + WholeHintPrefix + `0 instances`},
		},
		{
			"many keys are cut with a count",
			`variable "m" {
  default = { alpha = 1, bravo = 2, charlie = 3, delta = 4, echo = 5, foxtrot = 6, golf = 7 }
}
data "x" "y" {
  for_each = var.m
}
`,
			[]string{`5 for_each ` + WholeHintPrefix + `alpha, bravo, charlie, delta, … 3 more`},
		},
		{
			"one instance",
			`variable "n" {
  default = 1
}
resource "terraform_data" "c" {
  count = var.n
}
`,
			[]string{`5 count ` + WholeHintPrefix + `1 instance`},
		},
		{
			"a literal count gets no hint",
			`resource "terraform_data" "c" {
  count = 2
}
`,
			[]string{},
		},
		{
			"a null for_each is rejected, so its reference gets no hint either",
			`variable "m" {
  default = null
}
resource "terraform_data" "c" {
  for_each = var.m
}
`,
			[]string{},
		},
		{
			"a dynamic block's for_each is an ordinary argument",
			`variable "rules" {
  default = ["a", "b"]
}
resource "terraform_data" "c" {
  dynamic "rule" {
    for_each = var.rules
    content {}
  }
}
`,
			[]string{`6 var.rules ` + ValueHintPrefix + `["a", "b"]`},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"main.tf": tc.config})
			ev := treeEvaluator(t, root, nil)
			got := hintStrings(ev.InlayHintsWith("main.tf", hcl.Range{}, InlayOptions{Policy: InlayInformative, MaxLength: 40}))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected hints (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInlayHintsWith_rangeAndCap(t *testing.T) {
	root := writeTree(t, map[string]string{"main.tf": `variable "a" {
  default = "a"
}
locals {
  l = [var.a, var.a, var.a, var.a, var.a]
}
resource "terraform_data" "r" {
  input = [var.a, var.a, var.a, var.a, var.a]
  count = var.a == "a" ? 2 : 0
}
`})
	ev := treeEvaluator(t, root, nil)
	all := ev.InlayHintsWith("main.tf", hcl.Range{}, InlayOptions{Policy: InlayInformative, MaxPerLine: 2})
	if diff := cmp.Diff([]string{
		`5 l ` + WholeHintPrefix + `["a", "a", "a", "a", "a"]`,
		`8 var.a ` + ValueHintPrefix + `"a"`,
		`8 var.a ` + ValueHintPrefix + `"a"`,
		`9 count ` + WholeHintPrefix + `2 instances`,
	}, hintStrings(all)); diff != "" {
		t.Errorf("unexpected hints (-want +got):\n%s", diff)
	}

	// Only what lies inside the range: line 9, where the count hint is.
	f := ev.Module().Files["main.tf"]
	start := strings.Index(string(f.Bytes), "  count")
	rng := hcl.Range{Filename: "main.tf",
		Start: hcl.Pos{Line: 9, Column: 1, Byte: start},
		End:   hcl.Pos{Line: 9, Column: 40, Byte: len(f.Bytes) - 3}}
	if diff := cmp.Diff([]string{`9 count ` + WholeHintPrefix + `2 instances`},
		hintStrings(ev.InlayHintsWith("main.tf", rng, InlayOptions{Policy: InlayInformative}))); diff != "" {
		t.Errorf("unexpected hints in range (-want +got):\n%s", diff)
	}
}

func TestIsLibraryRoot(t *testing.T) {
	module := `variable "create" {
  default = false
}
locals {
  created = var.create ? "yes" : "no"
}
`
	testCases := []struct {
		name    string
		files   map[string]string
		indexed bool
		want    bool
	}{
		{"examples directory", map[string]string{"main.tf": module, "examples/basic/main.tf": `module "m" {
  source = "../../"
}
`}, false, true},
		{"called from below only", map[string]string{"main.tf": module, "wrappers/main.tf": `module "m" {
  source = "../"
}
`}, true, true},
		{"a .terraform directory is no sign of a root", map[string]string{"main.tf": module, "examples/a/main.tf": "", ".terraform/modules/modules.json": "{}"}, false, true},
		{"tfvars make a root", map[string]string{"main.tf": module, "examples/a/main.tf": "", "terraform.tfvars": "create = true\n"}, false, false},
		{"a backend makes a root", map[string]string{"main.tf": module + "terraform {\n  backend \"local\" {}\n}\n", "examples/a/main.tf": ""}, false, false},
		{"local state makes a root", map[string]string{"main.tf": module, "examples/a/main.tf": "", "terraform.tfstate": "{}"}, false, false},
		{"no examples and no callers", map[string]string{"main.tf": module}, false, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			var indexed func(string) []Caller
			if tc.indexed {
				wrappers, err := LoadModule(osFS{}, filepath.Join(root, "wrappers"))
				if err != nil {
					t.Fatal(err)
				}
				indexed = func(dir string) []Caller {
					if dir == root {
						return []Caller{{Parent: wrappers, Name: "m"}}
					}
					return nil
				}
			}
			ev := treeEvaluator(t, root, indexed)
			got := IsLibraryRoot(osFS{}, ev.Module(), indexed)
			if got != tc.want {
				t.Fatalf("IsLibraryRoot = %v, want %v", got, tc.want)
			}

			// A library's defaults give no hints; a root's do.
			hints := hintStrings(ev.InlayHintsWith("main.tf", hcl.Range{}, InlayOptions{Policy: InlayInformative, HideDefaults: got}))
			want := []string{`5 created ` + WholeHintPrefix + `"no"`}
			if tc.name == "tfvars make a root" {
				want = []string{`5 created ` + WholeHintPrefix + `"yes"`}
			}
			if got {
				want = []string{}
			}
			if diff := cmp.Diff(want, hints); diff != "" {
				t.Errorf("unexpected hints (-want +got):\n%s", diff)
			}
			// the hover still shows the default
			src := string(ev.Module().Files["main.tf"].Bytes)
			at := strings.Index(src, "var.create ?") + 5
			if h, ok := ev.HoverAt("main.tf", hcl.Pos{Line: 5, Column: 18, Byte: at}, treeEnv); !ok || !strings.Contains(h.Content, "default") {
				t.Errorf("no variable hover after the defaults were hidden: %v", h)
			}
		})
	}
}
