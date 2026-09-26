// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func mustParseExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	return expr
}

func hoverContent(h *Hover) string {
	if h == nil {
		return "<nil>"
	}
	return h.Content
}

// fixedInputs chooses the same inputs for every module.
type fixedInputs Inputs

func (f fixedInputs) Inputs(string) Inputs { return Inputs(f) }

// inputsEvaluator loads the module in dir with the inputs in, with its
// callers, the way the language server does.
func inputsEvaluator(t *testing.T, dir string, in Inputs) *Evaluator {
	t.Helper()
	fsys := WithInputs(osFS{}, fixedInputs(in))
	mod, err := LoadModule(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	AddCallers(fsys, mod, 3, nil)
	return NewEvaluator(mod)
}

func inputsEnv(in Inputs) Env {
	fsys := WithInputs(osFS{}, fixedInputs(in))
	return Env{LoadModule: func(dir string) (*Module, error) { return LoadModule(fsys, dir) }}
}

// TestInputs_precedence checks the values of variables against tofu
// console (OpenTofu 1.12.6) with the same files and flags: the default,
// then TF_VAR_, terraform.tfvars, terraform.tfvars.json, the
// *.auto.tfvars files in lexical order, and the -var-file files in the
// order given.
func TestInputs_precedence(t *testing.T) {
	main := `variable "a" {
  type    = string
  default = "default"
}
variable "obj" {
  type    = object({ n = number, tags = list(string) })
  default = { n = 0, tags = [] }
}
variable "n" {
  type    = number
  default = 0
}
variable "untyped" {
  default = "u"
}
variable "anything" {
  type    = any
  default = "x"
}
`
	full := map[string]string{
		"main.tf":                main,
		"terraform.tfvars":       `a = "tfvars"`,
		"terraform.tfvars.json":  `{"a": "tfvars-json"}`,
		"b.auto.tfvars":          `a = "auto-b"`,
		"a.auto.tfvars.json":     `{"a": "auto-a-json"}`,
		"envs/prod.tfvars":       `a = "prod"`,
		"envs/stage.tfvars":      `a = "stage"`,
		"envs/stage.auto.tfvars": `a = "never loaded by itself"`,
	}
	bare := map[string]string{
		"main.tf":          main,
		"envs/prod.tfvars": `a = "prod"`,
	}
	tfvarsOnly := map[string]string{
		"main.tf":               main,
		"terraform.tfvars":      `a = "tfvars"`,
		"terraform.tfvars.json": `{"a": "tfvars-json"}`,
	}
	testCases := []struct {
		name  string
		tree  map[string]string
		in    Inputs
		vname string
		// want is the value, "unknown" when there is none, or "refused"
		// when OpenTofu refuses to run
		want   string
		source string
	}{
		{"auto files only", full, Inputs{}, "a", `"auto-b"`, "b.auto.tfvars"},
		{"terraform.tfvars.json after terraform.tfvars", tfvarsOnly, Inputs{}, "a", `"tfvars-json"`, "terraform.tfvars.json"},
		{"a -var-file overrides every auto file", full, Inputs{VarFiles: []string{"envs/prod.tfvars"}}, "a", `"prod"`, "envs/prod.tfvars"},
		{"the last -var-file wins", full, Inputs{VarFiles: []string{"envs/prod.tfvars", "envs/stage.tfvars"}}, "a", `"stage"`, "envs/stage.tfvars"},
		{"the order of -var-file matters", full, Inputs{VarFiles: []string{"envs/stage.tfvars", "envs/prod.tfvars"}}, "a", `"prod"`, "envs/prod.tfvars"},
		{"an auto file in a subdirectory is not loaded by itself", full, Inputs{}, "a", `"auto-b"`, "b.auto.tfvars"},
		{"TF_VAR_ is below every tfvars file", full, Inputs{EnvVars: map[string]string{"a": "fromenv"}}, "a", `"auto-b"`, "b.auto.tfvars"},
		{"TF_VAR_ is above the default", bare, Inputs{EnvVars: map[string]string{"a": "fromenv"}}, "a", `"fromenv"`, "TF_VAR_a"},
		{"a -var-file is above TF_VAR_", bare, Inputs{VarFiles: []string{"envs/prod.tfvars"}, EnvVars: map[string]string{"a": "fromenv"}}, "a", `"prod"`, "envs/prod.tfvars"},
		{"TF_VAR_ of a string keeps quotes as text", bare, Inputs{EnvVars: map[string]string{"a": `"q"`}}, "a", `"\"q\""`, "TF_VAR_a"},
		{"TF_VAR_ of a number converts", bare, Inputs{EnvVars: map[string]string{"n": "7"}}, "n", `7`, "TF_VAR_n"},
		{"TF_VAR_ of a number with spaces is invalid", bare, Inputs{EnvVars: map[string]string{"n": " 7 "}}, "n", "unknown", ""},
		{"TF_VAR_ of an object is HCL", bare, Inputs{EnvVars: map[string]string{"obj": `{n=3, tags=["x"]}`}}, "obj", `{ n = 3, tags = ["x"] }`, "TF_VAR_obj"},
		{"TF_VAR_ of an untyped variable is a string", bare, Inputs{EnvVars: map[string]string{"untyped": "[1,2]"}}, "untyped", `"[1,2]"`, "TF_VAR_untyped"},
		{"TF_VAR_ of type any is HCL", bare, Inputs{EnvVars: map[string]string{"anything": "[1,2]"}}, "anything", `[1, 2]`, "TF_VAR_anything"},
		{"TF_VAR_ of type any that is not HCL is refused", bare, Inputs{EnvVars: map[string]string{"anything": "hello"}}, "a", "refused", ""},
		{"TF_VAR_ of an undeclared variable is ignored", bare, Inputs{EnvVars: map[string]string{"zzz": "1"}}, "n", `0`, "default"},
		{"a missing -var-file is refused", bare, Inputs{VarFiles: []string{"envs/nope.tfvars"}}, "a", "refused", ""},
		{"a -var-file outside the module is not read", bare, Inputs{VarFiles: []string{"../outside.tfvars", "/etc/x.tfvars"}}, "a", `"default"`, "default"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.tree)
			ev := inputsEvaluator(t, root, tc.in)
			r := ev.Eval(mustParseExpr(t, "var."+tc.vname), nil)
			switch tc.want {
			case "refused":
				if r.Kind != Rejected || !strings.Contains(r.Reason, "refuses to run") {
					t.Fatalf("expected a refusal, got %#v", r)
				}
				return
			case "unknown":
				if r.IsKnown() {
					t.Fatalf("expected no value, got %#v", r.Value)
				}
				return
			}
			if !r.IsKnown() {
				t.Fatalf("expected %s, got %#v", tc.want, r)
			}
			if got := FormatCompact(r.Value, 100); got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
			v, _ := ev.Variable(tc.vname)
			if _, source, _ := v.Effective(); source != tc.source {
				t.Fatalf("expected the value from %q, got %q", tc.source, source)
			}
		})
	}
}

// TestVariableFailures checks where validation failures are reported
// against tofu plan (OpenTofu 1.12.6) on the same configurations: the
// variables it rejects, and those it does not.
func TestVariableFailures(t *testing.T) {
	rootTree := map[string]string{
		"main.tf": `variable "stage" {
  type = string
  validation {
    condition     = contains(["dev", "staging", "prod"], var.stage)
    error_message = "The stage must be dev, staging or prod, not ${var.stage}."
  }
}
variable "region" {
  type    = string
  default = "moon"
  validation {
    condition     = startswith(var.region, "eu-")
    error_message = "Only EU regions."
  }
}
variable "min" {
  type    = number
  default = 1
}
variable "max" {
  type    = number
  default = 10
  validation {
    condition     = var.max >= var.min
    error_message = "max must be at least min."
  }
}
variable "fnerr" {
  type    = string
  default = "b"
  validation {
    condition     = regex("^a", var.fnerr) == "a"
    error_message = "must start with a"
  }
}
variable "fine" {
  type    = string
  default = "ok"
  validation {
    condition     = length(var.fine) > 0
    error_message = "not empty"
  }
}
locals { allowed = ["x", "y"] }
variable "withlocal" {
  type    = string
  default = "z"
  validation {
    condition     = contains(local.allowed, var.withlocal)
    error_message = "must be in local.allowed"
  }
}
variable "unknown" {
  type = string
  validation {
    condition     = var.unknown != ""
    error_message = "never known"
  }
}
variable "secret" {
  type      = string
  default   = "secret-bad"
  sensitive = true
  validation {
    condition     = length(var.secret) < 3
    error_message = "too long: ${var.secret}"
  }
}
`,
		"terraform.tfvars": "stage = \"prd\"\nmin   = 5\nmax   = 3\n",
	}
	// only b is reported: OpenTofu skips a's rules, and c's through a
	// local, since they refer to b, whose value fails
	crossTree := map[string]string{
		"main.tf": `variable "b" {
  type    = number
  default = 5
  validation {
    condition     = var.b < 3
    error_message = "b must be below 3"
  }
}
variable "a" {
  type    = number
  default = 1
  validation {
    condition     = var.a > var.b
    error_message = "a must exceed b"
  }
  validation {
    condition     = var.a > 10
    error_message = "a must exceed 10"
  }
}
locals { lb = var.b * 2 }
variable "c" {
  type    = number
  default = 1
  validation {
    condition     = var.c > local.lb
    error_message = "c must exceed local.lb"
  }
}
variable "other" {
  type    = number
  default = 1
  validation {
    condition     = var.b > 100
    error_message = "a rule that does not test its own variable is rejected otherwise"
  }
}
`,
	}
	envTree := map[string]string{
		"main.tf": `variable "stage" {
  type = string
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod."
  }
}
`,
		"terraform.tfvars":  "stage = \"dev\"\n",
		"envs/prod.tfvars":  "stage = \"prd\"\n",
		"envs/fixed.tfvars": "stage = \"prod\"\n",
	}
	callTree := map[string]string{
		"main.tf": `module "child" {
  source = "./child"
  name   = "BAD"
}
module "child2" {
  source = "./child"
  name   = "good"
}
module "kids" {
  source   = "./child"
  for_each = toset(["a", "B"])
  name     = each.key
}
module "unknown" {
  source = "./child"
  name   = terraform_data.x.output
}
resource "terraform_data" "x" {}
`,
		"child/main.tf": `variable "name" {
  type = string
  validation {
    condition     = lower(var.name) == var.name
    error_message = "name must be lower case"
  }
}
variable "size" {
  type    = number
  default = 100
  validation {
    condition     = var.size < 50
    error_message = "size must be below 50"
  }
}
output "name" { value = var.name }
`,
	}

	testCases := []struct {
		name string
		tree map[string]string
		dir  string
		in   Inputs
		want []string
	}{
		{
			"root module: tfvars values, defaults, other variables, locals and function errors",
			rootTree, "", Inputs{},
			[]string{
				`main.tf:10:13 default: Only EU regions.`,
				`main.tf:30:13 default: error: Error in function call: Call to function "regex" failed: pattern did not match any part of the given string.`,
				`main.tf:47:13 default: must be in local.allowed`,
				`main.tf:62:15 default: (the error message includes a sensitive value, so it is not shown)`,
				`terraform.tfvars:1:9 terraform.tfvars: The stage must be dev, staging or prod, not prd.`,
				`terraform.tfvars:3:9 terraform.tfvars: max must be at least min.`,
			},
		},
		{
			"rules that refer to a variable whose value fails are skipped",
			crossTree, "", Inputs{},
			[]string{`main.tf:3:13 default: b must be below 3`},
		},
		{
			"the automatically loaded value passes",
			envTree, "", Inputs{},
			nil,
		},
		{
			"the selected environment's value fails",
			envTree, "", Inputs{VarFiles: []string{"envs/prod.tfvars"}},
			[]string{`envs/prod.tfvars:1:9 envs/prod.tfvars: The stage must be dev or prod.`},
		},
		{
			"a later -var-file fixes it",
			envTree, "", Inputs{VarFiles: []string{"envs/prod.tfvars", "envs/fixed.tfvars"}},
			nil,
		},
		{
			"a TF_VAR_ value that no tfvars file overrides",
			map[string]string{"main.tf": envTree["main.tf"]}, "", Inputs{EnvVars: map[string]string{"stage": "prd"}},
			[]string{`main.tf:1:1 TF_VAR_stage: The stage must be dev or prod.`},
		},
		{
			"module call arguments, per instance, against the called module's rules",
			callTree, "", Inputs{},
			[]string{
				`main.tf:12:14 argument module.kids["B"]: name must be lower case`,
				`main.tf:3:12 argument module.child: name must be lower case`,
			},
		},
		{
			"the default a caller leaves unset, from the child module",
			callTree, "child", Inputs{},
			[]string{
				`child/main.tf:10:13 default module.child,module.child2,module.kids["B"],module.kids["a"],module.unknown: size must be below 50`,
			},
		},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			root := writeTree(t, tc.tree)
			ev := inputsEvaluator(t, filepath.Join(root, tc.dir), tc.in)
			var got []string
			for _, f := range ev.VariableFailures(inputsEnv(tc.in)) {
				rel, _ := filepath.Rel(root, f.Range.Filename)
				var msgs []string
				for _, fail := range f.Failures {
					msg := fail.Message
					if fail.Err {
						msg = "error: " + msg
					}
					msgs = append(msgs, msg)
				}
				where := f.Source
				if len(f.Calls) > 0 {
					calls := append([]string{}, f.Calls...)
					sort.Strings(calls)
					where += " " + strings.Join(calls, ",")
				}
				got = append(got, fmt.Sprintf("%s:%d:%d %s: %s", filepath.ToSlash(rel), f.Range.Start.Line, f.Range.Start.Column, where, strings.Join(msgs, " | ")))
			}
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("expected:\n%s\n\ngot:\n%s", strings.Join(tc.want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

// TestConditionFailures checks preconditions, postconditions and check
// assertions against tofu plan (OpenTofu 1.12.6) on the same
// configuration.
func TestConditionFailures(t *testing.T) {
	condTree := map[string]string{
		"main.tf": `variable "size" {
  type    = number
  default = 100
}
variable "names" {
  type    = list(string)
  default = ["a", "b"]
}
locals {
  limit = 50
}
resource "terraform_data" "pre" {
  input = var.size
  lifecycle {
    precondition {
      condition     = var.size <= local.limit
      error_message = "size ${var.size} is above the limit."
    }
  }
}
resource "terraform_data" "post" {
  input = "x"
  lifecycle {
    postcondition {
      condition     = var.size < 10
      error_message = "post: size too big."
    }
  }
}
resource "terraform_data" "selfpost" {
  input = "x"
  lifecycle {
    postcondition {
      condition     = self.input == "y"
      error_message = "self: input is not y."
    }
  }
}
resource "terraform_data" "zero" {
  count = 0
  lifecycle {
    precondition {
      condition     = var.size < 0
      error_message = "zero instances: never evaluated."
    }
  }
}
resource "terraform_data" "each" {
  for_each = toset(var.names)
  lifecycle {
    precondition {
      condition     = each.key != "b"
      error_message = "each: ${each.key} is not allowed."
    }
  }
}
resource "terraform_data" "okpre" {
  lifecycle {
    precondition {
      condition     = var.size > 0
      error_message = "never shown"
    }
  }
}
output "out" {
  value = var.size
  precondition {
    condition     = var.size < 20
    error_message = "output: size too big."
  }
}
check "chk" {
  assert {
    condition     = var.size < 30
    error_message = "check: size too big."
  }
}
check "chkok" {
  assert {
    condition     = var.size > 30
    error_message = "never shown"
  }
}
`,
	}
	skipTree := map[string]string{
		"main.tf": `variable "size" {
  type    = number
  default = 100
}
resource "terraform_data" "a" {
  input = 1
}
resource "terraform_data" "b" {
  lifecycle {
    precondition {
      condition     = terraform_data.a.input == 2
      error_message = "resource attributes are never evaluated"
    }
    precondition {
      condition     = terraform.workspace == "prod"
      error_message = "the workspace depends on how tofu runs"
    }
    precondition {
      condition     = true
      error_message = "a condition that refers to nothing is rejected otherwise"
    }
    precondition {
      condition     = regex("^a", "b") == "a" && var.size > 0
      error_message = "an error with known values"
    }
  }
}
data "terraform_remote_state" "s" {
  backend = "local"
  lifecycle {
    postcondition {
      condition     = var.size < 10
      error_message = "a data source may be read during apply"
    }
  }
}
module "never" {
  source = "./child"
  count  = 0
}
module "twice" {
  source = "./child2"
  for_each = toset(["a", "b"])
  size     = each.key == "a" ? 1 : 100
}
`,
		"child/main.tf": `locals {
  fixed = "x"
}
check "c" {
  assert {
    condition     = local.fixed == "y"
    error_message = "the module is never created"
  }
}
`,
		"child2/main.tf": `variable "size" {
  type = number
}
check "c" {
  assert {
    condition     = var.size < 10
    error_message = "the calls disagree"
  }
}
`,
	}
	testCases := []struct {
		name string
		tree map[string]string
		dir  string
		want []string
	}{
		{
			"preconditions, postconditions, outputs, instances and checks",
			condTree, "",
			[]string{
				`main.tf:16:23 precondition resource terraform_data.pre: size 100 is above the limit.`,
				`main.tf:25:23 postcondition resource terraform_data.post: post: size too big.`,
				`main.tf:52:23 precondition resource terraform_data.each["b"]: each: b is not allowed.`,
				`main.tf:68:21 precondition output output.out: output: size too big.`,
				`main.tf:74:21 assert check check.chk: check: size too big.`,
			},
		},
		{
			"resources, the workspace, data postconditions and constant conditions are skipped",
			skipTree, "",
			[]string{
				`main.tf:23:23 precondition resource terraform_data.b: error: Error in function call: Call to function "regex" failed: pattern did not match any part of the given string.`,
			},
		},
		{
			"a module that is never created",
			skipTree, "child",
			nil,
		},
		{
			"a module whose calls disagree",
			skipTree, "child2",
			nil,
		},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			root := writeTree(t, tc.tree)
			ev := inputsEvaluator(t, filepath.Join(root, tc.dir), Inputs{})
			var got []string
			for _, f := range ev.ConditionFailures() {
				rel, _ := filepath.Rel(root, f.Range.Filename)
				msg := f.Message
				if f.Err {
					msg = "error: " + msg
				}
				got = append(got, fmt.Sprintf("%s:%d:%d %s %s %s: %s", filepath.ToSlash(rel), f.Range.Start.Line, f.Range.Start.Column, f.Kind, f.BlockType, f.Address, msg))
			}
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("expected:\n%s\n\ngot:\n%s", strings.Join(tc.want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

func TestInputsStore(t *testing.T) {
	s := NewInputsStore()
	changed, env := s.Set(map[string][]string{
		"/w/root":  {"envs/prod.tfvars", "./extra.tfvars", "../escape.tfvars", "/abs.tfvars"},
		"/w/other": {"../x.tfvars"},
	}, nil)
	if strings.Join(changed, ",") != "/w/root" || env {
		t.Fatalf("unexpected change: %q %v", changed, env)
	}
	if got := s.Inputs("/w/root/").VarFiles; strings.Join(got, ",") != "envs/prod.tfvars,extra.tfvars" {
		t.Fatalf("unexpected var files: %q", got)
	}
	if got := s.Selecting("/w/root/envs"); strings.Join(got, ",") != "/w/root" {
		t.Fatalf("unexpected modules choosing envs: %q", got)
	}
	if dir, name, ok := s.ModuleOf("/w/root/envs/prod.tfvars"); !ok || dir != "/w/root" || name != "envs/prod.tfvars" {
		t.Fatalf("unexpected module of the file: %q %q %v", dir, name, ok)
	}
	if _, _, ok := s.ModuleOf("/w/root/envs/stage.tfvars"); ok {
		t.Fatal("a file nobody chose has a module")
	}
	changed, env = s.Set(map[string][]string{"/w/root": {"envs/prod.tfvars", "extra.tfvars"}}, map[string]string{"a": "1"})
	if len(changed) != 0 || !env {
		t.Fatalf("unexpected change: %q %v", changed, env)
	}
	changed, _ = s.Set(nil, map[string]string{"a": "1"})
	if strings.Join(changed, ",") != "/w/root" {
		t.Fatalf("unexpected change: %q", changed)
	}
	if got := EnvVarsFrom([]string{"TF_VAR_a=1", "TF_VAR_=2", "HOME=/h", "TF_VAR_b=x=y"}); len(got) != 2 || got["a"] != "1" || got["b"] != "x=y" {
		t.Fatalf("unexpected environment: %#v", got)
	}
}

// TestHover_selectedEnvironment checks that hovers name the source of a
// value from the selected environment or the environment, and never show
// a sensitive one.
func TestHover_selectedEnvironment(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "stage" {
  type = string
}
variable "token" {
  type      = string
  sensitive = true
}
locals {
  s = var.stage
  t = var.token
}
`,
		"terraform.tfvars": "stage = \"dev\"\n",
		"envs/prod.tfvars": "stage = \"prod\"\n",
	})
	in := Inputs{VarFiles: []string{"envs/prod.tfvars"}, EnvVars: map[string]string{"token": "hunter2"}}
	ev := inputsEvaluator(t, root, in)
	h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "var.stage", 5), inputsEnv(in))
	if !ok || !strings.Contains(h.Content, "**Value** `\"prod\"` from `envs/prod.tfvars` (selected environment)") ||
		!strings.Contains(h.Content, "Overrides: `terraform.tfvars` sets `\"dev\"`") {
		t.Fatalf("unexpected hover: %v %s", ok, hoverContent(h))
	}
	h, ok = ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "var.token", 5), inputsEnv(in))
	if !ok || !strings.Contains(h.Content, "from `TF_VAR_token` (environment)") || strings.Contains(h.Content, "hunter2") {
		t.Fatalf("unexpected hover: %v %s", ok, hoverContent(h))
	}

	f, _ := parseFile(filepath.Join(root, "envs", "prod.tfvars"), []byte("stage = \"prod\"\n"))
	h, ok = ev.VarsFileHover("envs/prod.tfvars", f, hcl.Pos{Line: 1, Column: 3, Byte: 2})
	if !ok || !strings.Contains(h.Content, "Selected environment: OpenTofu applies this file with `-var-file=envs/prod.tfvars`.") ||
		!strings.Contains(h.Content, "**This value is used**") {
		t.Fatalf("unexpected hover: %v %s", ok, hoverContent(h))
	}
	f, _ = parseFile(filepath.Join(root, "terraform.tfvars"), []byte("stage = \"dev\"\n"))
	h, ok = ev.VarsFileHover("terraform.tfvars", f, hcl.Pos{Line: 1, Column: 3, Byte: 2})
	if !ok || !strings.Contains(h.Content, "**Overridden** by `envs/prod.tfvars` (selected environment), which sets `\"prod\"`.") {
		t.Fatalf("unexpected hover: %v %s", ok, hoverContent(h))
	}
	if r := ev.EvalLocal("t"); !r.IsKnown() || !r.IsSensitive() {
		t.Fatalf("expected a known sensitive value, got %#v", r)
	}
	if v := ev.EvalLocal("s").Value; !v.RawEquals(cty.StringVal("prod")) {
		t.Fatalf("unexpected local: %#v", v)
	}
}

// TestDiagnosticsEvaluator_rootEvidence checks that diagnostics count the
// defaults of a root module only when the module shows it is run as it
// is. A library module, initialized for its examples, often leaves to
// its callers what its defaults and preconditions reject (as aws-ia's vpc
// module does with az_count and azs).
func TestDiagnosticsEvaluator_rootEvidence(t *testing.T) {
	main := `variable "azs" {
  type    = list(string)
  default = null
}
variable "stage" {
  type    = string
  default = "qa"
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "The stage must be dev or prod."
  }
}
resource "terraform_data" "x" {
  lifecycle {
    precondition {
      condition     = var.azs != null
      error_message = "Callers must set azs."
    }
  }
}
`
	testCases := []struct {
		name  string
		files map[string]string
		in    Inputs
		want  string
	}{
		{"no sign of a root run", map[string]string{".terraform/modules/modules.json": "{}"}, Inputs{}, ""},
		{"a -var-file that is not loaded by itself", map[string]string{"probe.tfvars": ""}, Inputs{}, ""},
		{"terraform.tfvars", map[string]string{"terraform.tfvars": ""}, Inputs{}, "stage precondition"},
		{"an auto tfvars file", map[string]string{"x.auto.tfvars.json": "{}"}, Inputs{}, "stage precondition"},
		{"a backend", map[string]string{"backend.tf": "terraform {\n  backend \"local\" {}\n}\n"}, Inputs{}, "stage precondition"},
		{"a chosen environment", map[string]string{"envs/prod.tfvars": ""}, Inputs{VarFiles: []string{"envs/prod.tfvars"}}, "stage precondition"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"main.tf": main}
			for k, v := range tc.files {
				files[k] = v
			}
			root := writeTree(t, files)
			fsys := WithInputs(osFS{}, fixedInputs(tc.in))
			mod, err := LoadModule(fsys, root)
			if err != nil {
				t.Fatal(err)
			}
			ev := NewDiagnosticsEvaluatorContext(context.Background(), mod)
			var got []string
			for _, f := range ev.VariableFailures(inputsEnv(tc.in)) {
				got = append(got, f.Variable)
			}
			for _, f := range ev.ConditionFailures() {
				got = append(got, f.Kind)
			}
			if strings.Join(got, " ") != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, strings.Join(got, " "))
			}
			// hovers still show the defaults
			if r := NewEvaluator(mod).Eval(mustParseExpr(t, "var.azs"), nil); !r.IsKnown() || !r.Value.IsNull() {
				t.Fatalf("the hover evaluator lost the default: %#v", r)
			}
		})
	}
}
