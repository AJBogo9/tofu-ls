// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty-debug/ctydebug"
	"github.com/zclconf/go-cty/cty"
)

type osFS struct{}

func (osFS) ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }

func testEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := LoadModule(osFS{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	ev := NewEvaluator(mod)
	ev.SetEnv(Env{
		LoadModule: func(dir string) (*Module, error) { return LoadModule(osFS{}, dir) },
		Attribute: func(blockType, typeName, attr string) (*AttributeInfo, bool) {
			if blockType == "resource" && typeName == "random_pet" && attr == "id" {
				return &AttributeInfo{Type: "string", Computed: true, Description: "The random pet name.", DocsURL: "https://example.com/pet"}, true
			}
			if blockType == "resource" && typeName == "local_sensitive_file" && attr == "content" {
				return &AttributeInfo{Type: "string", Required: true, Sensitive: true}, true
			}
			if blockType == "resource" && typeName == "random_pet" && attr == "prefix" {
				return &AttributeInfo{Type: "string", Optional: true}, true
			}
			return nil, false
		},
	})
	return ev
}

func TestVarsFileOrder(t *testing.T) {
	got := VarsFileOrder([]string{"b.auto.tfvars", "terraform.tfvars.json", "a.auto.tfvars.json", "terraform.tfvars", "z.auto.tfvars"})
	want := []string{"terraform.tfvars", "terraform.tfvars.json", "a.auto.tfvars.json", "b.auto.tfvars", "z.auto.tfvars"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected order: %s", diff)
	}
}

func TestVariable_Effective(t *testing.T) {
	ev := testEvaluator(t)
	testCases := []struct {
		name       string
		wantValue  cty.Value
		wantSource string
		wantOK     bool
	}{
		{"project", cty.StringVal("demo"), "terraform.tfvars", true},
		// b.auto.tfvars sorts after a.auto.tfvars.json and wins.
		{"stage", cty.StringVal("prod"), "b.auto.tfvars", true},
		{"network", cty.ObjectVal(map[string]cty.Value{
			"cidr":        cty.StringVal("10.0.0.0/16"),
			"subnet_bits": cty.NumberIntVal(8),
			"zones":       cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		}), "default", true},
		{"required", cty.NilVal, "", false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := ev.Variable(tc.name)
			if !ok {
				t.Fatalf("variable %q not decoded", tc.name)
			}
			val, source, ok := v.Effective()
			if ok != tc.wantOK {
				t.Fatalf("expected ok=%t, got %t", tc.wantOK, ok)
			}
			if !ok {
				return
			}
			if source != tc.wantSource {
				t.Fatalf("expected source %q, got %q", tc.wantSource, source)
			}
			if diff := cmp.Diff(tc.wantValue, val, ctydebug.CmpOptions); diff != "" {
				t.Fatalf("unexpected value: %s", diff)
			}
		})
	}
}

func TestEvalLocal(t *testing.T) {
	ev := testEvaluator(t)
	testCases := []struct {
		local      string
		wantKind   Kind
		wantValue  cty.Value
		wantReason string
		sensitive  bool
	}{
		{"prefix", Known, cty.StringVal("demo-prod"), "", false},
		{"upper", Known, cty.StringVal("DEMO-PROD"), "", false},
		{"subnets", Known, cty.ObjectVal(map[string]cty.Value{
			"a": cty.StringVal("10.0.0.0/24"),
			"b": cty.StringVal("10.0.1.0/24"),
		}), "", false},
		{"pet", AfterApply, cty.NilVal, "random_pet.p.id", false},
		{"started", AfterApply, cty.NilVal, "timestamp()", false},
		{"yaml", NotEvaluated, cty.NilVal, "yamlencode()", false},
		{"hashed", Known, cty.NilVal, "", true},
		{"missing", NoInput, cty.NilVal, "var.required", false},
		{"cycle_a", NotEvaluated, cty.NilVal, "local.cycle_a refers to itself", false},
		{"motd", AtPlan, cty.NilVal, "data.local_file.motd.content", false},
		{"from_mod", Known, cty.NumberIntVal(42), "", false},
		{"ws", Known, cty.StringVal("default"), "", false},
		{"file", Known, cty.StringVal("hello\n"), "", false},
	}
	for _, tc := range testCases {
		t.Run(tc.local, func(t *testing.T) {
			r := ev.EvalLocal(tc.local)
			if r.Kind != tc.wantKind {
				t.Fatalf("expected kind %d, got %d (%s)", tc.wantKind, r.Kind, r.Reason)
			}
			if tc.wantReason != "" && r.Reason != tc.wantReason {
				t.Fatalf("expected reason %q, got %q", tc.wantReason, r.Reason)
			}
			if r.IsSensitive() != tc.sensitive {
				t.Fatalf("expected sensitive=%t", tc.sensitive)
			}
			if tc.wantValue != cty.NilVal {
				if diff := cmp.Diff(tc.wantValue, r.Value, ctydebug.CmpOptions); diff != "" {
					t.Fatalf("unexpected value: %s", diff)
				}
			}
		})
	}
}

// posOf returns the position of the nth occurrence of needle in a module
// file, plus offset bytes.
func posOf(t *testing.T, ev *Evaluator, file, needle string, offset int) hcl.Pos {
	t.Helper()
	src := string(ev.Module().Files[file].Bytes)
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("%q not found in %s", needle, file)
	}
	i += offset
	line := strings.Count(src[:i], "\n") + 1
	col := i - strings.LastIndex(src[:i], "\n")
	return hcl.Pos{Line: line, Column: col, Byte: i}
}

func TestHoverAt(t *testing.T) {
	testCases := []struct {
		name    string
		file    string
		needle  string
		offset  int
		want    []string
		notWant []string
	}{
		{
			"var with tfvars overrides",
			"locals.tf", "var.stage", 5,
			[]string{
				"`var.stage` _string_",
				"**Value** `\"prod\"` from `b.auto.tfvars`",
				"Overrides: `a.auto.tfvars.json` sets `\"qa\"`; `terraform.tfvars` sets `\"staging\"`; default `\"dev\"`",
				"_May be overridden by `-var` or `-var-file`._",
			},
			nil,
		},
		{
			"var declaration label with validation",
			"variables.tf", `"project"`, 2,
			[]string{"`var.project` _string_", "Project name.", "1 validation rule", "from `terraform.tfvars`"},
			nil,
		},
		{
			"sensitive var never shows its value",
			"locals.tf", "var.secret", 5,
			[]string{"**Value** (sensitive) from the default", "sensitive"},
			[]string{"hunter2"},
		},
		{
			"object var with optional attribute",
			"locals.tf", "var.network.cidr", 13,
			[]string{"`var.network.cidr` = `\"10.0.0.0/16\"`", "subnet_bits = 8", "optional(number, 8)"},
			[]string{"dynamic"},
		},
		{
			"required var without value",
			"locals.tf", "var.required", 5,
			[]string{"**Value** unknown: no default and no tfvars value. Set it with `-var`, `-var-file` or `TF_VAR_required`."},
			nil,
		},
		{
			"local with function",
			"main.tf", "local.prefix", 7,
			[]string{"`local.prefix` _string_", "**Value** `\"demo-prod\"`", "prefix = format(\"%s-%s\", var.project, var.stage)"},
			nil,
		},
		{
			"local known after apply",
			"locals.tf", "pet ", 1,
			[]string{"**Value** known after apply: depends on `random_pet.p.id`"},
			nil,
		},
		{
			"each.value over a map",
			"main.tf", "each.value", 6,
			[]string{"`each.value` in `terraform_data.svc`", "2 instances", "- `\"db\"`: `5432`", "- `\"web\"`: `443`"},
			nil,
		},
		{
			"count.index",
			"main.tf", "count.index", 7,
			[]string{"`count.index` in `terraform_data.worker`", "`count = 2`: 2 instances", "- `0`", "- `1`"},
			nil,
		},
		{
			"module output with the call's inputs",
			"locals.tf", "module.child.doubled", 15,
			[]string{"output of `./modules/child`", "Twice the number.", "**Value** `42`", "value = var.number * 2"},
			nil,
		},
		{
			"module call label lists inputs, required first",
			"main.tf", `module "child"`, 9,
			[]string{"**module** `child` · source `./modules/child`", "- `name` _string_ · required = `\"demo-prod\"`", "- `number` _number_ · required = `21`", "- `unused` _string_ · default `\"u\"`", "- `doubled`: Twice the number."},
			nil,
		},
		{
			"output label with its value",
			"main.tf", `output "sensitive_prefix"`, 9,
			[]string{"**output** `sensitive_prefix`", "**Value** (sensitive)", "value = local.upper", "sensitive"},
			[]string{"DEMO-PROD"},
		},
		{
			"output label with a known value",
			"main.tf", `output "plain_prefix"`, 9,
			[]string{"**output** `plain_prefix`", "The name prefix.", "**Value** `\"demo-prod\"`"},
			nil,
		},
		{
			"sensitive for_each is never listed",
			"main.tf", "each.value\n}\n\nmodule", 6,
			[]string{"`for_each = toset([var.secret])` is sensitive"},
			[]string{"hunter2"},
		},
		{
			"module call with a sensitive input",
			"main.tf", `module "child_secret"`, 9,
			[]string{"- `name` _string_ · required = (sensitive)"},
			[]string{"hunter2"},
		},
		{
			"computed resource attribute",
			"locals.tf", "random_pet.p.id", 13,
			[]string{"`random_pet.p.id` _string_", "computed: **known after apply**", "The random pet name.", "[`random_pet` documentation](https://example.com/pet)"},
			nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := testEvaluator(t)
			pos := posOf(t, ev, tc.file, tc.needle, tc.offset)
			h, ok := ev.HoverAt(tc.file, pos, *ev.host)
			if !ok {
				t.Fatalf("no hover at %#v", pos)
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Content, w) {
					t.Errorf("hover lacks %q:\n%s", w, h.Content)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(h.Content, w) {
					t.Errorf("hover must not contain %q:\n%s", w, h.Content)
				}
			}
		})
	}
}

func TestHoverAt_childModule(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := LoadModule(osFS{}, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := LoadModule(osFS{}, filepath.Join(root, "modules", "child"))
	if err != nil {
		t.Fatal(err)
	}
	child.RootPath = root
	child.Callers = []Caller{{Parent: parent, Name: "child"}, {Parent: parent, Name: "child2"}}
	ev := NewEvaluator(child)

	testCases := []struct {
		needle string
		want   []string
	}{
		{"var.number", []string{"**Value** by module call:", "- `module.child` (`../../main.tf`): `21`", "- `module.child2` (`../../main.tf`): `4`"}},
		{"var.name", []string{"**Value** `\"demo-prod\"` in all 2 module calls"}},
		{"label =", []string{"**Value** by module call:", "- `module.child`: `\"demo-prod-21\"`", "- `module.child2`: `\"demo-prod-4\"`"}},
		{"count.index", []string{"`count = var.number > 5 ? 2 : 1` differs between module calls:", "- `module.child`: 2 instances `[0, 1]`", "- `module.child2`: 1 instance `[0]`"}},
	}
	for _, tc := range testCases {
		t.Run(tc.needle, func(t *testing.T) {
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", tc.needle, 2), Env{})
			if !ok {
				t.Fatal("no hover")
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Content, w) {
					t.Errorf("hover lacks %q:\n%s", w, h.Content)
				}
			}
		})
	}
}

func TestInlayHints(t *testing.T) {
	ev := testEvaluator(t)
	hints := ev.InlayHints("locals.tf", hcl.Range{}, 20)
	got := make([]string, 0, len(hints))
	for _, h := range hints {
		got = append(got, h.Ref+" "+h.Label)
	}
	for _, w := range []string{
		`var.project = "demo"`,
		`local.prefix = "demo-prod"`,
		`var.network.zones = ["a", "b"]`,
	} {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing hint %q in %q", w, got)
		}
	}
	for _, g := range got {
		if strings.Contains(g, "hunter2") || strings.HasPrefix(g, "var.secret") {
			t.Errorf("sensitive value in a hint: %q", g)
		}
		if strings.HasPrefix(g, "var.required") || strings.HasPrefix(g, "local.cycle") {
			t.Errorf("hint for an unknown value: %q", g)
		}
	}
}

func TestFormatCompact(t *testing.T) {
	testCases := []struct {
		val  cty.Value
		max  int
		want string
	}{
		{cty.StringVal("css-prod"), 40, `"css-prod"`},
		{cty.StringVal("a-very-long-string-value"), 12, `"a-very-lo…"`},
		{cty.NumberIntVal(42), 40, `42`},
		{cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}), 40, `["a", "b"]`},
		{cty.ListVal([]cty.Value{cty.StringVal("alpha"), cty.StringVal("beta"), cty.StringVal("gamma"), cty.StringVal("delta")}), 24, `["alpha", … 3 more]`},
		{cty.ObjectVal(map[string]cty.Value{"web": cty.NumberIntVal(443), "worker": cty.NumberIntVal(9000)}), 40, `{ web = 443, worker = 9000 }`},
		{cty.ObjectVal(map[string]cty.Value{"web": cty.StringVal("a long value here"), "worker": cty.StringVal("another long value")}), 30, `{ web = …, worker = … }`},
		{cty.StringVal("secret").Mark(SensitiveMark), 40, `(sensitive)`},
		{cty.UnknownVal(cty.String), 40, `(known after apply)`},
	}
	for _, tc := range testCases {
		t.Run(tc.want, func(t *testing.T) {
			got := FormatCompact(tc.val, tc.max)
			if got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}

func TestFormatValue(t *testing.T) {
	val := cty.ObjectVal(map[string]cty.Value{
		"a":      cty.StringVal("x"),
		"nested": cty.ObjectVal(map[string]cty.Value{"b": cty.NumberIntVal(1)}),
		"key 1":  cty.UnknownVal(cty.String),
	})
	want := `{
  a       = "x"
  "key 1" = (known after apply)
  nested  = { b = 1 }
}`
	if got := FormatValue(val); got != want {
		t.Fatalf("expected:\n%s\ngot:\n%s", want, got)
	}
	if got := FormatValue(cty.StringVal("line1\nline2\n")); got != "<<-EOT\n  line1\n  line2\nEOT" {
		t.Fatalf("unexpected heredoc: %q", got)
	}
	if got := FormatValue(cty.StringVal("${x}")); got != `"$${x}"` {
		t.Fatalf("unexpected escaping: %s", got)
	}
}

func TestVarsFileHover(t *testing.T) {
	ev := testEvaluator(t)
	testCases := []struct {
		name     string
		filename string
		src      string
		needle   string
		want     []string
	}{
		{
			"overridden value",
			"terraform.tfvars", "stage = \"staging\"\n", "stage",
			[]string{"`var.stage` _string_", "Matches the variable's type.", "**Overridden** by `b.auto.tfvars`, which sets `\"prod\"`."},
		},
		{
			"winning value",
			"b.auto.tfvars", "stage = \"prod\"\n", "stage",
			[]string{"**This value is used**"},
		},
		{
			"type mismatch",
			"terraform.tfvars", "network = \"oops\"\n", "network",
			[]string{"**Does not match the type:**"},
		},
		{
			"file loaded only with -var-file",
			"prod.tfvars", "stage = \"prod\"\n", "stage",
			[]string{"applies only with `-var-file=prod.tfvars`"},
		},
		{
			"undeclared variable",
			"terraform.tfvars", "nope = 1\n", "nope",
			[]string{"No variable `nope` is declared in this module"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := hclsyntax.ParseConfig([]byte(tc.src), tc.filename, hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			i := strings.Index(tc.src, tc.needle) + 1
			h, ok := ev.VarsFileHover(tc.filename, f, hcl.Pos{Line: 1, Column: i + 1, Byte: i})
			if !ok {
				t.Fatal("no hover")
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Content, w) {
					t.Errorf("hover lacks %q:\n%s", w, h.Content)
				}
			}
		})
	}
}

func TestValueBlock_json(t *testing.T) {
	got := valueBlock("**Value**", cty.StringVal(`{"a":[1,2]}`), "")
	want := "**Value** (a JSON string)\n```json\n{\n  \"a\": [\n    1,\n    2\n  ]\n}\n```\n\n"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if got := valueBlock("**Value**", cty.StringVal("{not json"), ""); got != "**Value** `\"{not json\"`\n\n" {
		t.Fatalf("unexpected rendering of a plain string: %q", got)
	}
}

func TestLocalCallers(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	callers := LocalCallers(osFS{}, filepath.Join(root, "modules", "child"), 4)
	got := make([]string, 0, len(callers))
	for _, c := range callers {
		got = append(got, c.Parent.Path+":"+c.Name)
	}
	want := []string{root + ":child", root + ":child2", root + ":child_secret"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected callers: %s", diff)
	}
	if callers := LocalCallers(osFS{}, filepath.Join(root, "modules", "child"), 1); len(callers) != 0 {
		t.Fatalf("expected no callers within one level, got %d", len(callers))
	}
}

func TestInstalledRoot(t *testing.T) {
	testCases := []struct {
		dir    string
		root   string
		wantOK bool
	}{
		{"/work/infra/.terraform/modules/vpc", "/work/infra", true},
		{"/work/infra/.terraform/modules/vpc/modules/subnet", "/work/infra", true},
		{"/work/infra/modules/vpc", "", false},
	}
	for _, tc := range testCases {
		t.Run(tc.dir, func(t *testing.T) {
			root, ok := InstalledRoot(filepath.FromSlash(tc.dir))
			if ok != tc.wantOK || root != filepath.FromSlash(tc.root) {
				t.Fatalf("expected (%q, %t), got (%q, %t)", tc.root, tc.wantOK, root, ok)
			}
		})
	}
}

func TestWantsHover(t *testing.T) {
	ev := testEvaluator(t)
	testCases := []struct {
		file   string
		needle string
		offset int
		want   bool
	}{
		{"locals.tf", "var.stage", 5, true},
		{"locals.tf", "local.prefix", 7, true},
		{"locals.tf", "prefix   =", 1, true},
		{"locals.tf", "random_pet.p.id", 13, true},
		{"locals.tf", "terraform.workspace", 12, true},
		{"locals.tf", "path.module", 6, false},
		{"locals.tf", "format(", 2, false},
		{"main.tf", `resource "random_pet"`, 2, false},
		{"main.tf", `"random_pet"`, 3, false},
		{"main.tf", `module "child"`, 9, true},
		{"main.tf", `"./modules/child"`, 4, true},
		{"variables.tf", `"project"`, 2, true},
		{"variables.tf", "description", 3, false},
		{"main.tf", `output "plain_prefix"`, 9, true},
	}
	for _, tc := range testCases {
		t.Run(tc.file+" "+tc.needle, func(t *testing.T) {
			f := ev.Module().Files[tc.file]
			if got := WantsHover(f, posOf(t, ev, tc.file, tc.needle, tc.offset)); got != tc.want {
				t.Fatalf("expected %t, got %t", tc.want, got)
			}
		})
	}
}

func TestInlayHints_redactedPlaces(t *testing.T) {
	ev := testEvaluator(t)
	src := string(ev.Module().Files["main.tf"].Bytes)
	lineOf := func(needle string) int {
		i := strings.Index(src, needle)
		if i < 0 {
			t.Fatalf("%q not found", needle)
		}
		return strings.Count(src[:i], "\n") + 1
	}
	hintLines := map[int]bool{}
	for _, h := range ev.InlayHints("main.tf", hcl.Range{}, 40) {
		hintLines[h.Pos.Line] = true
	}
	if !hintLines[lineOf("filename = local.upper")] {
		t.Error("expected a hint on a non-sensitive argument")
	}
	if hintLines[lineOf("content  = local.prefix")] {
		t.Error("no hint expected on an argument the provider marks sensitive")
	}
	if hintLines[lineOf("value     = local.upper")] {
		t.Error("no hint expected inside a sensitive output")
	}
}

func TestAddCallers_nested(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "nested"))
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name    string
		nesting int
		want    string
	}{
		// The caller of b is itself a child: its variable comes from the
		// root, not from its default.
		{"callers of callers", 3, "**Value** `\"hello from-root-via-a\"`"},
		// Without the root, a's variable is unknown, never its default.
		{"one level", 1, "- `module.b`: set by the module calls: depends on `var.x`"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mod, err := LoadModule(osFS{}, filepath.Join(root, "modules", "a", "b"))
			if err != nil {
				t.Fatal(err)
			}
			AddCallers(osFS{}, mod, tc.nesting, nil)
			if mod.RootPath == "" {
				t.Fatal("expected b to be a child module")
			}
			ev := NewEvaluator(mod)
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", "greeting", 2), Env{})
			if !ok {
				t.Fatal("no hover")
			}
			if !strings.Contains(h.Content, tc.want) {
				t.Fatalf("hover lacks %q:\n%s", tc.want, h.Content)
			}
			if strings.Contains(h.Content, "a-default") {
				t.Fatalf("hover shows the default of a caller that is itself called:\n%s", h.Content)
			}
		})
	}
}
