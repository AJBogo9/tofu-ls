// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// TestHoverAt_malformedBlocks hovers every position of blocks as they look
// while being typed or renamed, where the labels do not match what the
// block type declares. None may panic, and the static hover leaves them
// to the schema hover.
func TestHoverAt_malformedBlocks(t *testing.T) {
	testCases := []struct {
		name string
		src  string
	}{
		{"variable with two labels", "variable \"new\" \"old\" {\n  default = 1\n}\nlocals {\n  y = var.new\n}\n"},
		{"labeled locals", "locals \"x\" {\n  a = 1\n}\n\nlocals {\n  y = local.a\n}\n"},
		{"module without a label", "variable \"x\" {\n  default = \"a\"\n}\n\nmodule {\n  source = \"./child\"\n}\n"},
		{"module with two labels", "module \"a\" \"b\" {\n  source = \"./child\"\n}\nlocals {\n  o = module.a.out\n}\n"},
		{"output with two labels", "output \"a\" \"b\" {\n  value = 1\n}\n"},
		{"output without a label", "output {\n  value = 1\n}\n"},
		{"resource with one label", "resource \"terraform_data\" {\n  input = each.value\n  for_each = toset([\"a\"])\n}\n"},
		{"data without labels", "data {\n  x = count.index\n  count = 2\n}\nlocals {\n  d = data.x.y.z\n}\n"},
		{"null module source", "variable \"src\" {\n  type    = string\n  default = null\n}\nmodule \"m\" {\n  source = var.src\n}\nlocals {\n  o = module.m.out\n}\n"},
		{"for_each set with null", "resource \"terraform_data\" \"s\" {\n  for_each = toset([\"a\", null])\n  input    = each.key\n}\nmodule \"n\" {\n  source   = \"./child\"\n  for_each = toset([\"a\", null])\n}\nlocals {\n  o = module.n[\"a\"].out\n}\n"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"main.tf":       tc.src,
				"child/main.tf": "variable \"v\" {\n  default = 1\n}\noutput \"out\" {\n  value = var.v\n}\n",
			})
			ev := treeEvaluator(t, root, nil)
			lines := strings.Split(tc.src, "\n")
			for i, line := range lines {
				for col := 1; col <= len(line)+1; col++ {
					pos := hcl.Pos{Line: i + 1, Column: col, Byte: byteOffset(lines, i, col)}
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("hover at %d:%d panicked: %v", pos.Line, pos.Column, r)
							}
						}()
						h, ok := ev.HoverAt("main.tf", pos, treeEnv)
						if ok && strings.HasPrefix(line[:col-1], "variable \"new\"") {
							t.Fatalf("hover on a variable that is not declared: %s", h.Content)
						}
					}()
				}
			}
			ev.InlayHints("main.tf", hcl.Range{}, 40)
		})
	}
}

func byteOffset(lines []string, line, col int) int {
	n := 0
	for i := 0; i < line; i++ {
		n += len(lines[i]) + 1
	}
	return n + col - 1
}

func TestHoverAt_neverLie(t *testing.T) {
	nullableTree := map[string]string{
		"main.tf": `module "c" {
  source = "./child"
  d      = null
}
module "r" {
  source = "./child"
  d      = "set"
  r      = null
}
locals {
  from_null = module.c.d
}
`,
		"child/main.tf": `variable "d" {
  type     = string
  default  = "dflt"
  nullable = false
}
variable "r" {
  type     = string
  nullable = false
  default  = "r-default"
}
variable "req" {
  type     = string
  nullable = false
}
output "d" {
  value = var.d
}
`,
	}
	requiredNullTree := map[string]string{
		"main.tf": `module "c" {
  source = "./child"
  req    = null
}
`,
		"child/main.tf": `variable "req" {
  type     = string
  nullable = false
}
locals {
  l = var.req
}
`,
	}
	allTrueTree := map[string]string{
		"main.tf": `variable "subnets" {
  type = list(object({
    name   = string
    public = optional(bool)
  }))
  default = [{ name = "a", public = true }, { name = "b" }]
}
locals {
  all_public = alltrue([for s in var.subnets : s.public])
}
`,
	}
	pathTree := map[string]string{
		"main.tf": `module "app" {
  source = "./modules/app"
}
locals {
  pm = path.module
  pr = path.root
  pc = path.cwd
}
`,
		"modules/app/main.tf": `locals {
  pm = path.module
  pr = path.root
  motd = file("${path.module}/motd.txt")
}
`,
		"modules/app/motd.txt": "hello",
		".terraform/modules/g/main.tf": `locals {
  pm = path.module
}
`,
	}
	ignoreTree := map[string]string{
		"main.tf": `variable "x" {
  default = "one"
}
resource "terraform_data" "r" {
  input = var.x
  lifecycle {
    ignore_changes = [input]
  }
}
resource "terraform_data" "all" {
  input = "all-input"
  lifecycle {
    ignore_changes = all
  }
}
resource "terraform_data" "other" {
  input = "kept-input"
  lifecycle {
    ignore_changes = [triggers_replace]
  }
}
locals {
  l     = terraform_data.r.input
  l_all = terraform_data.all.input
  kept  = terraform_data.other.input
}
`,
		"terraform.tfvars": "x = \"two\"\n",
	}
	validationTree := map[string]string{
		"main.tf": `variable "v" {
  type    = string
  default = "bad"
  validation {
    condition     = var.v == "good"
    error_message = "Must be good, got ${var.v}."
  }
}
variable "ok" {
  default = "fine"
  validation {
    condition     = length(var.ok) > 1
    error_message = "Too short."
  }
}
variable "later" {
  default = "x"
  validation {
    condition     = terraform_data.t.id != ""
    error_message = "Depends on apply."
  }
}
variable "secret" {
  default   = "s"
  sensitive = true
  validation {
    condition     = length(var.secret) > 3
    error_message = "Secret ${var.secret} is short."
  }
}
resource "terraform_data" "t" {}
locals {
  l      = var.v
  ok     = var.ok
  later  = var.later
}
`,
	}
	callValidationTree := map[string]string{
		"main.tf": `module "c" {
  source = "./child"
  stage  = "prd"
}
locals {
  s = module.c.stage
}
`,
		"child/main.tf": `variable "stage" {
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod."
  }
}
output "stage" {
  value = var.stage
}
`,
	}
	fnTfvarsTree := map[string]string{
		"main.tf": `variable "a" {
  default = "dflt-a"
}
variable "b" {
  default = "dflt-b"
}
locals {
  a = var.a
  b = var.b
}
`,
		"terraform.tfvars": "a = upper(\"x\")\n",
	}
	syntaxTfvarsTree := map[string]string{
		"main.tf": `variable "a" {
  default = "dflt-a"
}
variable "b" {
  default = "dflt-b"
}
locals {
  a = var.a
  b = var.b
}
`,
		"terraform.tfvars": "a = \"ok\"\n",
		"x.auto.tfvars":    "b = \"from-broken\"\nc = [\n",
	}
	ephemeralTree := map[string]string{
		"main.tf": `variable "e" {
  type      = string
  default   = "eph-val"
  ephemeral = true
}
locals {
  l = var.e
  u = upper(nonsensitive(var.e))
  n = length(var.e)
}
`,
	}
	nullMetaTree := map[string]string{
		"main.tf": `variable "m" {
  type    = map(string)
  default = null
}
resource "terraform_data" "x" {
  for_each = var.m
  input    = each.value
}
resource "terraform_data" "y" {
  count = null
  input = count.index
}
`,
	}
	unsetTree := map[string]string{
		"main.tf": `resource "google_compute_instance" "vm" {
  name = "vm"
}
resource "terraform_data" "d" {}
locals {
  fwd = google_compute_instance.vm.can_ip_forward
  in  = terraform_data.d.input
}
`,
	}
	unsetEnv := treeEnv
	unsetEnv.Attribute = func(blockType, typeName, attr string) (*AttributeInfo, bool) {
		if blockType == "resource" && typeName == "google_compute_instance" && attr == "can_ip_forward" {
			return &AttributeInfo{Type: "bool", Optional: true}, true
		}
		return treeEnv.Attribute(blockType, typeName, attr)
	}

	testCases := []struct {
		name    string
		tree    map[string]string
		dir     string
		file    string
		needle  string
		env     *Env
		want    []string
		notWant []string
	}{
		// nullable = false in module calls
		{"null argument takes the default", nullableTree, "", "main.tf", "from_null", nil, []string{"**Value** `\"dflt\"`"}, []string{"`null`"}},
		{"child hover says the default replaces null", nullableTree, "child", "main.tf", `"d"`, nil, []string{"- `module.c` (`../main.tf`): default `\"dflt\"`, since the call passes `null` and the variable is not nullable"}, nil},
		{"null argument with no default is rejected", requiredNullTree, "child", "main.tf", "  l =", nil, []string{"error: OpenTofu rejects module.c: it sets the variable `req`, which is not nullable and has no default, to null"}, nil},
		// alltrue over an unset optional attribute
		{"alltrue with a null element", allTrueTree, "", "main.tf", "all_public", nil, []string{"**Value** `false`"}, []string{"`true`"}},
		// path values as OpenTofu gives them
		{"path.module in the root", pathTree, "", "main.tf", "path.module", nil, []string{"**Value** `\".\"`"}, []string{"/"}},
		{"path.root in the root", pathTree, "", "main.tf", "path.root", nil, []string{"**Value** `\".\"`"}, nil},
		{"path.module in a child", pathTree, "modules/app", "main.tf", "path.module", nil, []string{"**Value** `\"modules/app\"`", "_For the root module `../..`._"}, nil},
		{"path.root in a child", pathTree, "modules/app", "main.tf", "path.root", nil, []string{"**Value** `\".\"`"}, nil},
		{"file functions still resolve from a child", pathTree, "modules/app", "main.tf", "motd =", nil, []string{"**Value** `\"hello\"`"}, nil},
		{"path.module in an installed module", pathTree, ".terraform/modules/g", "main.tf", "path.module", nil, []string{"**Value** `\".terraform/modules/g\"`"}, nil},
		// lifecycle.ignore_changes
		{"reference to an ignored argument", ignoreTree, "", "main.tf", "  l     =", nil, []string{"unknown: depends on `terraform_data.r.input`, and lifecycle.ignore_changes keeps the value from state"}, []string{"\"two\"", "\"one\""}},
		{"reference with ignore_changes = all", ignoreTree, "", "main.tf", "l_all", nil, []string{"lifecycle.ignore_changes keeps the value from state"}, []string{"all-input"}},
		{"argument not in ignore_changes", ignoreTree, "", "main.tf", "kept  =", nil, []string{"**Value** `\"kept-input\"`"}, nil},
		{"attribute hover on an ignored argument", ignoreTree, "", "main.tf", "terraform_data.r.input", nil, []string{"**Value** unknown: depends on `terraform_data.r.input`, and lifecycle.ignore_changes keeps the value from state", "Configured value `\"two\"`, which OpenTofu uses only when it creates the resource"}, nil},
		// validation
		{"variable that fails validation", validationTree, "", "main.tf", `"v"`, nil, []string{"**Value** rejected: `\"bad\"` from the default fails a validation rule, so OpenTofu refuses to plan:", "> Must be good, got bad."}, nil},
		{"local from a variable that fails validation", validationTree, "", "main.tf", "  l      =", nil, []string{"error: OpenTofu rejects the value of `var.v`, which fails its validation: \"Must be good, got bad.\""}, []string{"**Value** `\"bad\"`"}},
		{"variable that passes validation", validationTree, "", "main.tf", "  ok     =", nil, []string{"**Value** `\"fine\"`"}, nil},
		{"validation not known statically", validationTree, "", "main.tf", "  later  =", nil, []string{"**Value** `\"x\"`"}, []string{"rejected"}},
		{"sensitive value that fails validation", validationTree, "", "main.tf", `"secret"`, nil, []string{"**Value** rejected: (sensitive) from the default", "(the error message includes a sensitive value, so it is not shown)"}, []string{"Secret s", "`\"s\"`"}},
		{"module call value that fails validation", callValidationTree, "child", "main.tf", `"stage"`, nil, []string{"- `module.c` (`../main.tf`): error: OpenTofu rejects the value of `var.stage`, which fails its validation: \"The stage must be dev or prod.\""}, []string{"`\"prd\"`"}},
		{"module call input that fails validation", callValidationTree, "", "main.tf", `"c"`, nil, []string{"- `stage` _any_ · required · **error: OpenTofu rejects the value of `var.stage`"}, nil},
		{"output of a rejected module input", callValidationTree, "", "main.tf", "  s =", nil, []string{"OpenTofu rejects the value of `var.stage`"}, []string{"\"prd\""}},
		// tfvars files that OpenTofu rejects
		{"function call in tfvars", fnTfvarsTree, "", "main.tf", "  a =", nil, []string{"OpenTofu refuses to run: `terraform.tfvars` is invalid (line 1: Function calls not allowed: Functions may not be called here)"}, []string{"dflt-a"}},
		{"function call in tfvars refuses every variable", fnTfvarsTree, "", "main.tf", `"b"`, nil, []string{"**Value** none: OpenTofu refuses to run: `terraform.tfvars` is invalid"}, []string{"dflt-b"}},
		{"syntax error in tfvars", syntaxTfvarsTree, "", "main.tf", `"b"`, nil, []string{"**Value** none: OpenTofu refuses to run: `x.auto.tfvars` is invalid (line"}, []string{"from-broken", "dflt-b"}},
		{"syntax error in tfvars hides other files' values", syntaxTfvarsTree, "", "main.tf", "  a =", nil, []string{"OpenTofu refuses to run: `x.auto.tfvars` is invalid"}, []string{"\"ok\""}},
		// ephemeral values
		{"ephemeral variable", ephemeralTree, "", "main.tf", `"e"`, nil, []string{"**Value** (ephemeral value) from the default", "ephemeral"}, []string{"eph-val"}},
		{"local from an ephemeral variable", ephemeralTree, "", "main.tf", "  l =", nil, []string{"**Value** (ephemeral value)"}, []string{"eph-val"}},
		{"nonsensitive keeps a value ephemeral", ephemeralTree, "", "main.tf", "  u =", nil, []string{"**Value** (ephemeral value)"}, []string{"EPH-VAL"}},
		{"derived number stays ephemeral", ephemeralTree, "", "main.tf", "  n =", nil, []string{"**Value** (ephemeral value)"}, []string{"`7`"}},
		// null for_each and count
		{"null for_each", nullMetaTree, "", "main.tf", "each.value", nil, []string{"error: OpenTofu rejects a null `for_each`"}, []string{"0 instances"}},
		{"null count", nullMetaTree, "", "main.tf", "count.index", nil, []string{"error: OpenTofu rejects a null `count`"}, []string{"0 instances"}},
		// unset optional arguments
		{"unset optional argument may be defaulted", unsetTree, "", "main.tf", "google_compute_instance.vm.can_ip_forward", &unsetEnv, []string{"**Value** not set in the configuration; the provider may set a default, so **known after apply**"}, []string{"`null`"}},
		{"unset terraform_data input is null", unsetTree, "", "main.tf", "terraform_data.d.input", &unsetEnv, []string{"**Value** not set in the configuration (`null`)"}, nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.tree)
			env := treeEnv
			if tc.env != nil {
				env = *tc.env
			}
			ev := treeEvaluator(t, filepath.Join(root, filepath.FromSlash(tc.dir)), nil)
			ev.SetEnv(env)
			h, ok := ev.HoverAt(tc.file, posOf(t, ev, tc.file, tc.needle, 2), env)
			if !ok {
				t.Fatal("no hover")
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Content, w) {
					t.Errorf("hover lacks %q:\n%s", w, h.Content)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(h.Content, w) {
					t.Errorf("hover contains %q:\n%s", w, h.Content)
				}
			}
		})
	}
}

func TestInlayHints_neverLie(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "v" {
  default = "bad"
  validation {
    condition     = var.v == "good"
    error_message = "Must be good."
  }
}
variable "e" {
  default   = "eph-val"
  ephemeral = true
}
variable "fine" {
  default = "fine-val"
}
resource "terraform_data" "r" {
  input = "configured"
  lifecycle {
    ignore_changes = [input]
  }
}
locals {
  a = var.v
  b = var.e
  c = local.a
  d = terraform_data.r.input
  f = var.fine
}
output "o" {
  value = [var.v, var.e, local.d]
}
`,
	})
	ev := treeEvaluator(t, root, nil)
	var got []string
	for _, h := range ev.InlayHints("main.tf", hcl.Range{}, 40) {
		got = append(got, h.Ref+" "+h.Label)
	}
	for _, g := range got {
		for _, bad := range []string{"bad", "eph-val", "configured"} {
			if strings.Contains(g, bad) {
				t.Errorf("hint shows a value OpenTofu would not use: %q", g)
			}
		}
	}
	if !strings.Contains(strings.Join(got, "|"), "var.fine "+ValueHintPrefix+`"fine-val"`) {
		t.Errorf("missing the hint for a plain variable: %q", got)
	}
}

func TestValidate(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "stage" {
  type = string
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod, not ${var.stage}."
  }
  validation {
    condition     = length(var.stage) < 5
    error_message = "Too long."
  }
  validation {
    condition     = timestamp() != ""
    error_message = "Never known."
  }
}
variable "plain" {}
`,
	})
	ev := treeEvaluator(t, root, nil)
	testCases := []struct {
		name string
		val  cty.Value
		want []string
	}{
		{"stage", cty.StringVal("dev"), nil},
		{"stage", cty.StringVal("prd"), []string{"The stage must be dev or prod, not prd."}},
		{"stage", cty.StringVal("staging"), []string{"The stage must be dev or prod, not staging.", "Too long."}},
		{"stage", cty.UnknownVal(cty.String), nil},
		{"plain", cty.StringVal("x"), nil},
		{"missing", cty.StringVal("x"), nil},
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s=%#v", tc.name, tc.val), func(t *testing.T) {
			var got []string
			for _, f := range ev.Validate(tc.name, tc.val) {
				got = append(got, f.Message)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
	// Validate leaves the evaluator's own values alone.
	if r := ev.EvalLocal("x"); r.IsKnown() {
		t.Fatalf("unexpected local: %#v", r)
	}
	if v := ev.varValues["stage"]; v.IsKnown() {
		t.Fatalf("Validate changed var.stage to %#v", v)
	}
}

func TestVarsFileHover_neverLie(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "stage" {
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod."
  }
}
variable "e" {
  default   = "d"
  ephemeral = true
}
`,
		"terraform.tfvars":   "stage = \"prd\"\ne     = \"tfvars-eph\"\n",
		"z.auto.tfvars":      "e = \"auto-eph\"\n",
		"broken.auto.tfvars": "stage = \"dev\"\nx = [\n",
	})
	ev := treeEvaluator(t, root, nil)
	f, _ := parseFile(filepath.Join(root, "terraform.tfvars"), mustRead(t, filepath.Join(root, "terraform.tfvars")))
	testCases := []struct {
		needle  string
		want    []string
		notWant []string
	}{
		{"stage =", []string{"**Fails a validation rule**, so OpenTofu refuses to plan with it:", "> The stage must be dev or prod.", "**No value is used**: OpenTofu refuses to run: `broken.auto.tfvars` is invalid"}, nil},
		{"e     =", []string{"ephemeral"}, []string{"auto-eph"}},
	}
	for _, tc := range testCases {
		t.Run(tc.needle, func(t *testing.T) {
			idx := strings.Index(string(f.Bytes), tc.needle)
			pos := hcl.Pos{Line: strings.Count(string(f.Bytes[:idx]), "\n") + 1, Column: 2, Byte: idx + 1}
			h, ok := ev.VarsFileHover("terraform.tfvars", f, pos)
			if !ok {
				t.Fatal("no hover")
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Content, w) {
					t.Errorf("hover lacks %q:\n%s", w, h.Content)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(h.Content, w) {
					t.Errorf("hover contains %q:\n%s", w, h.Content)
				}
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withDeadline fails the test when fn runs longer than d, which is how an
// exponential or unbounded evaluation shows.
func withDeadline(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %s", d)
	}
}

func TestEvaluator_boundedWork(t *testing.T) {
	chain := func(kind string, n int) map[string]string {
		var b strings.Builder
		switch kind {
		case "resources":
			b.WriteString("resource \"terraform_data\" \"r0\" {\n  input = \"seed\"\n}\nresource \"terraform_data\" \"r1\" {\n  input = \"seed1\"\n}\n")
			for k := 2; k <= n; k++ {
				fmt.Fprintf(&b, "resource \"terraform_data\" \"r%d\" {\n  input = substr(\"${terraform_data.r%d.input}${terraform_data.r%d.input}\", 0, 4)\n}\n", k, k-1, k-2)
			}
			fmt.Fprintf(&b, "locals {\n  last = terraform_data.r%d.input\n}\n", n)
		case "data":
			b.WriteString("data \"local_file\" \"d0\" {\n  filename = \"x\"\n}\ndata \"local_file\" \"d1\" {\n  filename = \"x\"\n}\n")
			for k := 2; k <= n; k++ {
				fmt.Fprintf(&b, "data \"local_file\" \"d%d\" {\n  filename   = \"x\"\n  depends_on = [data.local_file.d%d, data.local_file.d%d]\n}\n", k, k-1, k-2)
			}
			fmt.Fprintf(&b, "locals {\n  last = data.local_file.d%d.content\n}\n", n)
		}
		return map[string]string{"main.tf": b.String()}
	}
	// The audit's case: a list of a million elements, then a for
	// expression that refers to it a thousand times, and many references.
	refs := make([]string, 1000)
	for i := range refs {
		refs[i] = fmt.Sprintf("  r%d = local.big", i)
	}
	bigTree := map[string]string{
		"main.tf": "locals {\n  a    = range(1000)\n  big  = flatten([for x in local.a : local.a])\n  many = [for x in local.a : local.big]\n" + strings.Join(refs, "\n") + "\n}\n",
	}

	t.Run("resource chain", func(t *testing.T) {
		ev := treeEvaluator(t, writeTree(t, chain("resources", 40)), nil)
		withDeadline(t, 10*time.Second, func() {
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "last =", 1), treeEnv)
			if !ok || !strings.Contains(h.Content, "**Value** `\"seed\"`") {
				t.Errorf("unexpected hover: %v", h)
			}
		})
	})
	t.Run("data chain", func(t *testing.T) {
		ev := treeEvaluator(t, writeTree(t, chain("data", 40)), nil)
		withDeadline(t, 10*time.Second, func() {
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "last =", 1), treeEnv)
			if !ok || !strings.Contains(h.Content, "known during the plan") {
				t.Errorf("unexpected hover: %v", h)
			}
		})
	})
	t.Run("a large value and many references", func(t *testing.T) {
		ev := treeEvaluator(t, writeTree(t, bigTree), nil)
		withDeadline(t, 20*time.Second, func() {
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "big  =", 1), treeEnv)
			if !ok || !strings.Contains(h.Content, "  0,\n") || !strings.Contains(h.Content, "… the rest is not shown") {
				t.Errorf("unexpected hover: %v", h)
			}
			h, ok = ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "many =", 1), treeEnv)
			if !ok || !strings.Contains(h.Content, "not evaluated: the value has more than 2000000 elements") {
				t.Errorf("unexpected hover: %v", h)
			}
			hints := ev.InlayHints("main.tf", hcl.Range{}, 40)
			n := 0
			for _, h := range hints {
				if h.Ref == "local.big" {
					n++
					if want := ValueHintPrefix + "[0, 1, 2, 3, 4, 5, 6, 7, … 999992 more]"; h.Label != want {
						t.Fatalf("expected %s, got %s", want, h.Label)
					}
				}
			}
			if n != 1001 {
				t.Fatalf("expected 1001 hints for local.big, got %d", n)
			}
		})
	})
}

func TestFormatCompact_stopsEarly(t *testing.T) {
	elems := make([]cty.Value, 200000)
	for i := range elems {
		elems[i] = cty.StringVal(strings.Repeat("x", 50))
	}
	list := cty.ListVal(elems)
	if got := FormatCompact(list, 40); got != "[200000 items]" {
		t.Fatalf("unexpected %q", got)
	}
	s, cut := formatValueCapped(list)
	if !cut || len(s) > maxRenderBytes+200 {
		t.Fatalf("rendered %d bytes, cut=%t", len(s), cut)
	}
	if got := FormatCompact(cty.StringVal(strings.Repeat("é", 100000)), 10); got != `"ééééééé…"` {
		t.Fatalf("unexpected %q", got)
	}
}

func TestEvaluator_cancelled(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": "variable \"v\" {\n  default = \"x\"\n}\nlocals {\n  a = var.v\n}\n",
	})
	mod, err := LoadModule(osFS{}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ev := NewEvaluatorContext(ctx, mod)
	if r := ev.EvalLocal("a"); !r.IsKnown() {
		t.Fatalf("expected a known value before cancelling, got %#v", r)
	}
	cancel()
	ev = NewEvaluatorContext(ctx, mod)
	if r := ev.EvalLocal("a"); r.Kind != NotEvaluated || !strings.Contains(r.Reason, "cancelled") {
		t.Fatalf("expected a cancelled evaluation, got %#v", r)
	}
	if hints := ev.InlayHints("main.tf", hcl.Range{}, 40); hints != nil {
		t.Fatalf("expected no hints once cancelled, got %v", hints)
	}
}
