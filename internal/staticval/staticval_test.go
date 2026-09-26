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
		"var.project " + ValueHintPrefix + `"demo"`,
		"local.prefix " + ValueHintPrefix + `"demo-prod"`,
		"var.network.zones " + ValueHintPrefix + `["a", "b"]`,
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
		{"locals.tf", "path.module", 6, true},
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

// writeTree writes files (relative path to content) under a new temporary
// directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// treeEvaluator loads the module in dir the way the language server does,
// with its callers and an environment that loads child modules.
func treeEvaluator(t *testing.T, dir string, indexed func(string) []Caller) *Evaluator {
	t.Helper()
	mod, err := LoadModule(osFS{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	AddCallers(osFS{}, mod, 3, indexed)
	ev := NewEvaluator(mod)
	ev.SetEnv(treeEnv)
	return ev
}

var treeEnv = Env{
	LoadModule: func(dir string) (*Module, error) { return LoadModule(osFS{}, dir) },
	Attribute: func(blockType, typeName, attr string) (*AttributeInfo, bool) {
		switch {
		case blockType == "data" && typeName == "local_file" && attr == "content":
			return &AttributeInfo{Type: "string", Computed: true}, true
		case blockType == "resource" && typeName == "random_password" && attr == "length":
			return &AttributeInfo{Type: "number", Required: true}, true
		case blockType == "resource" && typeName == "random_password" && attr == "result":
			return &AttributeInfo{Type: "string", Computed: true, Sensitive: true}, true
		case blockType == "resource" && typeName == "terraform_data" && attr == "input":
			return &AttributeInfo{Type: "dynamic", Optional: true}, true
		case blockType == "resource" && typeName == "local_sensitive_file" && attr == "content":
			return &AttributeInfo{Type: "string", Required: true, Sensitive: true}, true
		}
		return nil, false
	},
}

func TestHoverAt_evaluationSemantics(t *testing.T) {
	overrideTree := map[string]string{
		"main.tf": `variable "db_password" {
  type      = string
  sensitive = true
}
variable "size" {
  type    = number
  default = 1
}
locals {
  pw   = var.db_password
  name = "from-main"
  show = local.name
  sz   = var.size
}
`,
		"override.tf": `variable "db_password" {
  description = "The database password."
}
variable "size" {
  default = 5
}
`,
		"a_override.tf":    "locals {\n  name = \"from-override\"\n}\n",
		"terraform.tfvars": "db_password = \"OVERRIDE-LEAK-777\"\n",
	}
	childFilesTree := map[string]string{
		"main.tf": `module "one" {
  source = "./modules/child"
  name   = "alpha"
}
locals {
  r = module.one.greeting
}
`,
		"modules/child/main.tf": `variable "name" {}
locals {
  has_motd = fileexists("${path.module}/motd.txt")
  motd     = file("${path.module}/motd.txt")
  greeting = templatefile("${path.module}/greet.tftpl", { name = var.name })
  root_rel = file("${path.root}/modules/child/motd.txt")
}
output "greeting" {
  value = local.greeting
}
`,
		"modules/child/motd.txt":    "message of the day",
		"modules/child/greet.tftpl": "Hello, ${name}!",
	}
	dataTree := map[string]string{
		"main.tf": `resource "terraform_data" "a" {}
resource "random_pet" "p" {}
data "local_file" "dep" {
  filename   = "${path.module}/exists.txt"
  depends_on = [terraform_data.a]
}
data "local_file" "plain" {
  filename = "exists.txt"
}
data "local_file" "arg" {
  filename = "${random_pet.p.id}.txt"
}
locals {
  c_dep   = data.local_file.dep.content
  c_plain = data.local_file.plain.content
  c_arg   = data.local_file.arg.content
  pts     = plantimestamp()
}
resource "terraform_data" "per_line" {
  count = length(split("\n", data.local_file.dep.content))
  input = count.index
}
`,
		"exists.txt": "x",
	}
	fileTree := map[string]string{
		"main.tf": `locals {
  missing     = file("${path.module}/missing.txt")
  try_missing = try(file("${path.module}/missing.txt"), "fallback")
  can_missing = can(file("missing.txt"))
  outside     = file("/etc/hostname")
}
`,
	}
	callsTree := map[string]string{
		"main.tf": `module "one" {
  source = "./c"
  name   = "alpha"
}
module "two" {
  source = "./c"
  name   = "beta"
}
module "many" {
  source   = "./c"
  for_each = toset(["x", "y"])
  name     = each.key
}
`,
		"c/main.tf": `variable "name" {}
locals {
  label = "${var.name}-x"
}
`,
	}
	nestedTree := map[string]string{
		"main.tf": `module "one" {
  source = "./child"
}
module "many" {
  source   = "./child"
  for_each = toset(["x"])
  label    = each.key
}
module "counted" {
  source = "./child"
  count  = 2
}
locals {
  g = module.one.grand_out
  m = module.many["x"].grand_out
  c = module.counted[1].grand_out
}
`,
		"child/main.tf": `variable "label" {
  default = "alpha"
}
module "grand" {
  source = "./grand"
  label  = var.label
}
output "grand_out" {
  value = module.grand.shout
}
`,
		"child/grand/main.tf": `variable "label" {}
output "shout" {
  value = upper(var.label)
}
`,
	}
	sensitiveTree := map[string]string{
		"main.tf": `module "child_plain" {
  source   = "./child2"
  value_in = "public-a"
}
module "child_secret" {
  source   = "./child2"
  value_in = sensitive("public-a")
}
`,
		"child2/main.tf": `variable "value_in" {}
locals {
  v = "v-${var.value_in}"
}
`,
	}

	budgetTree := map[string]string{
		"main.tf": `locals {
  big_list = [for a in range(1000) : [for b in range(1000) : a * b]]
  small    = [for a in range(3) : [for b in range(2) : a * b]]
}
`,
	}
	cfgTree := map[string]string{
		"main.tf": `resource "random_password" "p" {
  length = 16
}
resource "terraform_data" "a" {
  input = "configured-input"
}
resource "terraform_data" "n" {
  count = 2
  input = "counted-input"
}
resource "local_sensitive_file" "s" {
  content = "hunter2"
}
locals {
  pw_len = random_password.p.length
  a_in   = terraform_data.a.input
  pw_out = random_password.p.result
  n_in   = terraform_data.n[0].input
  secret = local_sensitive_file.s.content
}
`,
	}
	nullTree := map[string]string{
		"main.tf": `variable "nn" {
  default  = "nn-default"
  nullable = false
}
locals {
  v = var.nn
}
`,
		"terraform.tfvars": "nn = null\n",
	}
	edgeTree := map[string]string{
		"main.tf": `variable "n_str" {
  type    = string
  default = "3"
}
resource "terraform_data" "counted" {
  count = var.n_str
  input = count.index
}
resource "terraform_data" "nums" {
  for_each = toset([1, 2])
  input    = each.value
}
`,
	}

	testCases := []struct {
		name    string
		tree    map[string]string
		dir     string
		needle  string
		want    []string
		notWant []string
	}{
		{"override keeps sensitive", overrideTree, "", "var.db_password", []string{sensitiveText}, []string{"OVERRIDE-LEAK-777", "_any_"}},
		{"override keeps the type", overrideTree, "", "var.size", []string{"_number_", "**Value** `5`"}, nil},
		{"override local wins", overrideTree, "", "local.name", []string{`"from-override"`}, []string{`"from-main"`}},
		{"child fileexists", childFilesTree, "modules/child", "has_motd", []string{"**Value** `true`"}, nil},
		{"child file", childFilesTree, "modules/child", "motd     =", []string{"message of the day"}, []string{"after apply"}},
		{"child templatefile", childFilesTree, "modules/child", "greeting =", []string{"Hello, alpha!"}, nil},
		{"child path.root", childFilesTree, "modules/child", "root_rel", []string{"message of the day"}, nil},
		{"root sees child output", childFilesTree, "", "  r =", []string{"Hello, alpha!"}, nil},
		{"data with depends_on", dataTree, "", "c_dep", []string{"known after apply", "`terraform_data.a`"}, []string{"during the plan"}},
		{"data read at plan", dataTree, "", "c_plain", []string{"known during the plan, which reads the data source"}, nil},
		{"data with unknown argument", dataTree, "", "c_arg", []string{"known after apply", "`random_pet.p.id`"}, nil},
		{"data attribute with depends_on", dataTree, "", "data.local_file.dep.content\n", []string{"computed: **known after apply**"}, []string{"read during the plan"}},
		{"data attribute read at plan", dataTree, "", "data.local_file.plain.content", []string{"computed: read during the plan"}, nil},
		{"plantimestamp", dataTree, "", "pts", []string{"known during the plan: depends on `plantimestamp()`"}, []string{"after apply"}},
		{"count known after apply", dataTree, "", "count.index", []string{"OpenTofu cannot plan a `count`"}, []string{"known once the plan"}},
		{"missing file", fileTree, "", "missing     =", []string{"not evaluated", "no file exists"}, []string{"after apply"}},
		{"try missing file", fileTree, "", "try_missing", []string{`"fallback"`}, nil},
		{"can missing file", fileTree, "", "can_missing", []string{"**Value** `false`"}, nil},
		{"file outside the module", fileTree, "", "outside", []string{"not evaluated: reads a file outside the module"}, []string{"after apply"}},
		{"per call with a for_each caller", callsTree, "c", "label =", []string{"- `module.one`: `\"alpha-x\"`", "- `module.two`: `\"beta-x\"`", "- `module.many`: not evaluated: module.many differs per instance"}, nil},
		{"nested module output", nestedTree, "", "  g =", []string{"**Value** `\"ALPHA\"`"}, []string{"after apply"}},
		{"module output of a for_each instance", nestedTree, "", "  m =", []string{"**Value** `\"X\"`"}, []string{"after apply"}},
		{"module output of a count instance", nestedTree, "", "  c =", []string{"**Value** `\"ALPHA\"`"}, []string{"after apply"}},
		{"reference to a for_each instance output", nestedTree, "", "module.many[\"x\"].grand_out", []string{"**Value** `\"X\"`"}, nil},
		{"count converts a string", edgeTree, "", "count.index", []string{"3 instances"}, []string{"must be a number"}},
		{"for_each rejects a set of numbers", edgeTree, "", "each.value", []string{"not evaluated: for_each must be a map or a set of strings, not set of number"}, []string{"2 instances"}},
		{"non-nullable null in tfvars", nullTree, "", "var.nn", []string{"**Value** `\"nn-default\"` from the default", "`terraform.tfvars` sets `null`, which the default replaces because the variable is not nullable"}, nil},
		{"child variable with a sensitive value", sensitiveTree, "child2", "var.value_in", []string{"`module.child_secret` (`../main.tf`): (sensitive)"}, []string{"`(sensitive)`"}},
		{"nested for expressions over a budget", budgetTree, "", "big_list", []string{"not evaluated: its for expressions run about 1000000 iterations"}, nil},
		{"nested for expressions within the budget", budgetTree, "", "small", []string{"**Value**", "[0, 0]"}, []string{"not evaluated"}},
		{"configured argument", cfgTree, "", "pw_len", []string{"**Value** `16`"}, []string{"after apply"}},
		{"configured optional argument", cfgTree, "", "a_in", []string{"**Value** `\"configured-input\"`"}, nil},
		{"computed attribute", cfgTree, "", "pw_out", []string{"known after apply: depends on `random_password.p.result`"}, nil},
		{"counted resource", cfgTree, "", "n_in", []string{"known after apply"}, []string{"counted-input"}},
		{"sensitive argument", cfgTree, "", "secret", []string{sensitiveText}, []string{"hunter2"}},
		{"equal call values keep sensitivity", sensitiveTree, "child2", "var.value_in", []string{"**Value** " + sensitiveText + " in all 2 module calls"}, nil},
		{"local of equal call values keeps sensitivity", sensitiveTree, "child2", "  v =", []string{sensitiveText}, []string{"v-public-a"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.tree)
			ev := treeEvaluator(t, filepath.Join(root, filepath.FromSlash(tc.dir)), nil)
			h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", tc.needle, 2), treeEnv)
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

func TestInlayHints_equalCallValuesKeepSensitivity(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `module "child_plain" {
  source   = "./child2"
  value_in = "public-a"
}
module "child_secret" {
  source   = "./child2"
  value_in = sensitive("public-a")
}
`,
		"child2/main.tf": `variable "value_in" {}
locals {
  v = "v-${var.value_in}"
  w = local.v
}
`,
	})
	ev := treeEvaluator(t, filepath.Join(root, "child2"), nil)
	for _, h := range ev.InlayHints("main.tf", hcl.Range{}, 40) {
		if strings.Contains(h.Label, "public-a") {
			t.Errorf("sensitive value in a hint: %s %s", h.Ref, h.Label)
		}
	}
}

func TestLoadModule_tofuFilesShadowTfFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf":   "locals {\n  both    = \"from-tf\"\n  only_tf = \"only-in-tf\"\n}\n",
		"main.tofu": "locals {\n  both = \"from-tofu\"\n}\n",
		"other.tf":  "locals {\n  other = 1\n}\n",
	})
	ev := treeEvaluator(t, root, nil)
	if _, ok := ev.Local("only_tf"); ok {
		t.Error("main.tf should be ignored when main.tofu exists")
	}
	if _, ok := ev.Local("other"); !ok {
		t.Error("other.tf has no .tofu twin and should be loaded")
	}
	if r := ev.EvalLocal("both"); !r.IsKnown() || r.Value.AsString() != "from-tofu" {
		t.Errorf("unexpected local.both: %#v", r)
	}
}

func TestAddCallers_rootWithTfvarsStaysRoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "name" {
  default = "default-name"
}
locals {
  greeting = "hello-${var.name}"
}
`,
		"terraform.tfvars": "name = \"root-tfvars\"\n",
		"examples/basic/main.tf": `module "this" {
  source = "../../"
  name   = "example"
}
`,
	})
	indexed := func(dir string) []Caller {
		parent, err := LoadModule(osFS{}, filepath.Join(root, "examples", "basic"))
		if err != nil {
			t.Fatal(err)
		}
		return []Caller{{Parent: parent, Name: "this"}}
	}
	ev := treeEvaluator(t, root, indexed)
	if ev.Module().RootPath != "" {
		t.Fatalf("a module with its own tfvars was classified as a child of %s", ev.Module().RootPath)
	}
	r := ev.EvalLocal("greeting")
	if !r.IsKnown() || r.Value.AsString() != "hello-root-tfvars" {
		t.Fatalf("unexpected local.greeting: %#v", r)
	}
}

func TestAddCallers_rootCalledByItsExamplesStaysRoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": `variable "name" {
  default = "default-name"
}
locals {
  greeting = "hello-${var.name}"
}
`,
		"examples/basic/main.tf": `module "this" {
  source = "../../"
  name   = "example"
}
`,
		"modules/app/main.tf": `variable "name" {}
`,
		"caller/main.tf": `module "app" {
  source = "../modules/app"
  name   = "from-caller"
}
`,
	})
	examples, err := LoadModule(osFS{}, filepath.Join(root, "examples", "basic"))
	if err != nil {
		t.Fatal(err)
	}
	caller, err := LoadModule(osFS{}, filepath.Join(root, "caller"))
	if err != nil {
		t.Fatal(err)
	}
	indexed := func(dir string) []Caller {
		switch dir {
		case root:
			return []Caller{{Parent: examples, Name: "this"}}
		case filepath.Join(root, "modules", "app"):
			return []Caller{{Parent: caller, Name: "app"}}
		}
		return nil
	}

	// no tfvars and no .terraform, called only from below: a root
	ev := treeEvaluator(t, root, indexed)
	if ev.Module().RootPath != "" {
		t.Fatalf("a module called only by its own examples was classified as a child of %s", ev.Module().RootPath)
	}
	r := ev.EvalLocal("greeting")
	if !r.IsKnown() || r.Value.AsString() != "hello-default-name" {
		t.Fatalf("unexpected local.greeting: %#v", r)
	}

	// a caller beside it still makes a module a child
	child := treeEvaluator(t, filepath.Join(root, "modules", "app"), indexed)
	if child.Module().RootPath != filepath.Join(root, "caller") {
		t.Fatalf("expected modules/app to be a child of caller, got root %q", child.Module().RootPath)
	}
}

func TestVarsFileHover_nonNullableNull(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf":          "variable \"nn\" {\n  default  = \"nn-default\"\n  nullable = false\n}\n",
		"terraform.tfvars": "nn = null\n",
	})
	ev := treeEvaluator(t, root, nil)
	f := ev.Module().VarsFiles["terraform.tfvars"]
	h, ok := ev.VarsFileHover("terraform.tfvars", f, hcl.Pos{Line: 1, Column: 2, Byte: 1})
	if !ok {
		t.Fatal("no hover")
	}
	want := "The variable is not nullable, so OpenTofu replaces this `null` with the default, `\"nn-default\"`."
	if !strings.Contains(h.Content, want) {
		t.Fatalf("hover lacks %q:\n%s", want, h.Content)
	}
	if strings.Contains(h.Content, "**Overridden** by `default`") {
		t.Fatalf("hover names the default like a file:\n%s", h.Content)
	}
}

func TestFormatValue_heredocRoundTrip(t *testing.T) {
	testCases := []string{
		"a\nb",
		"a\nb\n",
		"    x\n    y\n",
		"x\n  y\n",
		"first\nEOT\nlast\n",
		"first\nEOT\nEOF\nEND\n",
		"a\r\nb\n",
		"${var.x}\n%{if true}y\n",
		"\tx\ny\n",
		"x\n\ny\n",
	}
	for _, want := range testCases {
		t.Run(strings.ReplaceAll(want, "\n", "|"), func(t *testing.T) {
			for _, v := range []cty.Value{
				cty.StringVal(want),
				cty.ObjectVal(map[string]cty.Value{"k": cty.StringVal(want), "other": cty.NumberIntVal(1)}),
			} {
				rendered := FormatValue(v)
				src := "x = " + rendered + "\n"
				f, diags := hclsyntax.ParseConfig([]byte(src), "t.tf", hcl.InitialPos)
				if diags.HasErrors() {
					t.Fatalf("rendering does not parse: %s\n%s", diags, src)
				}
				attrs, _ := f.Body.JustAttributes()
				got, diags := attrs["x"].Expr.Value(nil)
				if diags.HasErrors() {
					t.Fatalf("rendering does not evaluate: %s\n%s", diags, src)
				}
				if got.Type().IsObjectType() {
					got = got.GetAttr("k")
				}
				if got.AsString() != want {
					t.Fatalf("rendering denotes %q, not %q:\n%s", got.AsString(), want, rendered)
				}
			}
		})
	}
}

func TestValueBlock_capsLargeValues(t *testing.T) {
	elems := make([]cty.Value, 1000)
	for i := range elems {
		elems[i] = cty.NumberIntVal(int64(i))
	}
	got := valueBlock("**Value**", cty.TupleVal(elems), "")
	if n := strings.Count(got, "\n"); n > maxValueLines+10 {
		t.Fatalf("hover value has %d lines", n)
	}
	if !strings.Contains(got, "more lines") {
		t.Fatalf("hover value does not say it was cut:\n%s", got)
	}
	long := valueBlock("**Value**", cty.StringVal(strings.Repeat("x", 100000)), "")
	if len(long) > maxValueChars+100 {
		t.Fatalf("hover value has %d bytes", len(long))
	}
}

func TestParseFile_cacheFollowsContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.tf")
	a, _ := parseFile(path, []byte("locals {\n  a = 1\n}\n"))
	again, _ := parseFile(path, []byte("locals {\n  a = 1\n}\n"))
	if a == nil || a != again {
		t.Fatal("the same content should reuse the parsed file")
	}
	b, _ := parseFile(path, []byte("locals {\n  a = 2\n}\n"))
	if b == a {
		t.Fatal("changed content must be parsed again")
	}
	attrs, _ := b.Body.(*hclsyntax.Body).Blocks[0].Body.JustAttributes()
	if v, _ := attrs["a"].Expr.Value(nil); !v.RawEquals(cty.NumberIntVal(2)) {
		t.Fatalf("unexpected value %#v", v)
	}
}
