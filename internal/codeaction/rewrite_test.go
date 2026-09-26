// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestUnwrapInterpolation(t *testing.T) {
	testCases := []struct {
		name   string
		src    string
		needle string
		title  string
		edits  []string
		want   string
	}{
		{
			name:   "an argument",
			src:    "resource \"local_file\" \"f\" {\n  filename = \"${var.path}\"\n}\n",
			needle: `"${var.path}"`,
			title:  `Replace "${var.path}" with var.path`,
			edits:  []string{`main.tf 2:14-2:27 "var.path"`},
			want:   "resource \"local_file\" \"f\" {\n  filename = var.path\n}\n",
		},
		{
			name:   "an operand keeps its precedence",
			src:    "locals {\n  x = \"${var.a ? 1 : 2}\" == \"1\"\n}\n",
			needle: `"${var.a ? 1 : 2}"`,
			title:  `Replace "${var.a ? 1 : 2}" with (var.a ? 1 : 2)`,
			edits:  []string{`main.tf 2:7-2:25 "(var.a ? 1 : 2)"`},
			want:   "locals {\n  x = (var.a ? 1 : 2) == \"1\"\n}\n",
		},
		{
			name:   "an operator needs no parentheses where it stands alone",
			src:    "locals {\n  x = [\"${var.a + 1}\"]\n}\n",
			needle: `"${var.a + 1}"`,
			title:  `Replace "${var.a + 1}" with var.a + 1`,
			edits:  []string{`main.tf 2:8-2:22 "var.a + 1"`},
			want:   "locals {\n  x = [var.a + 1]\n}\n",
		},
		{
			name:   "an object key stays an expression",
			src:    "locals {\n  x = { \"${local.k}\" = 1 }\n}\n",
			needle: `"${local.k}"`,
			title:  `Replace "${local.k}" with (local.k)`,
			edits:  []string{`main.tf 2:9-2:21 "(local.k)"`},
			want:   "locals {\n  x = { (local.k) = 1 }\n}\n",
		},
		{
			name:   "strip markers",
			src:    "locals {\n  x = \"${~ local.k ~}\"\n}\n",
			needle: `"${~ local.k ~}"`,
			title:  `Replace "${~ local.k ~}" with local.k`,
			edits:  []string{`main.tf 2:7-2:23 "local.k"`},
			want:   "locals {\n  x = local.k\n}\n",
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			fsys := files{"main.tf": tc.src}
			diag := Diagnostic{Code: CodeInterpolationOnly, Range: fsys.rangeOf(t, "main.tf", tc.needle, 1)}
			actions := QuickFixes(fsys.newEnv(), fsys.doc("main.tf"), diag)
			a := expectAction(t, fsys, actions, tc.title, tc.edits, files{"main.tf": tc.want})
			if !a.Preferred {
				t.Errorf("expected a preferred fix")
			}
		})
	}
}

func TestRunInit(t *testing.T) {
	fsys := files{"main.tf": "module \"m\" {\n  source = \"./m\"\n}\n"}
	diag := Diagnostic{Code: CodeModuleNotInstalled, Data: map[string]interface{}{"module": "m", "dir": "/ws"}, Range: fsys.rangeOf(t, "main.tf", `"./m"`, 1)}

	if actions := QuickFixes(fsys.newEnv(), fsys.doc("main.tf"), diag); len(actions) != 0 {
		t.Fatalf("a client without an init command gets no fix, got %#v", actions)
	}

	env := fsys.newEnv()
	env.InitCommand = "tofu.initCurrent"
	env.DirURI = func(dir string) string { return "file://" + dir }
	for _, code := range []string{CodeModuleNotInstalled, CodeProviderNotInstalled} {
		diag.Code = code
		actions := QuickFixes(env, fsys.doc("main.tf"), diag)
		if len(actions) != 1 {
			t.Fatalf("%s: expected 1 action, got %#v", code, actions)
		}
		a := actions[0]
		if a.Title != "Run tofu init" || !a.Preferred || len(a.Edits) != 0 || a.Command == nil ||
			a.Command.Name != "tofu.initCurrent" || fmt.Sprint(a.Command.Arguments) != "[file:///ws]" {
			t.Fatalf("%s: unexpected action %#v (command %#v)", code, a, a.Command)
		}
	}
}

func TestRewriteCall(t *testing.T) {
	testCases := []struct {
		name     string
		files    files
		needle   string
		offset   int
		varTypes map[string]cty.Type
		title    string
		disabled string
		edits    []string
		want     string
	}{
		{
			name:   "lookup without a default",
			files:  files{"main.tf": "locals {\n  x = lookup(var.m, \"k\")\n}\n"},
			needle: "lookup",
			title:  `Replace lookup() with var.m["k"]`,
			edits:  []string{`main.tf 2:7-2:25 "var.m[\"k\"]"`},
			want:   "locals {\n  x = var.m[\"k\"]\n}\n",
		},
		{
			name:   "lookup of a merge, with the cursor on the key",
			files:  files{"main.tf": "locals {\n  x = lookup(merge(a, b), local.key)\n}\n"},
			needle: "local.key",
			offset: 3,
			title:  "Replace lookup() with merge(a, b)[local.key]",
			edits:  []string{`main.tf 2:7-2:37 "merge(a, b)[local.key]"`},
			want:   "locals {\n  x = merge(a, b)[local.key]\n}\n",
		},
		{
			name:   "lookup of a conditional is parenthesized",
			files:  files{"main.tf": "locals {\n  x = lookup(var.a ? var.m : var.n, \"k\")\n}\n"},
			needle: "lookup",
			title:  `Replace lookup() with (var.a ? var.m : var.n)["k"]`,
			edits:  []string{`main.tf 2:7-2:41 "(var.a ? var.m : var.n)[\"k\"]"`},
			want:   "locals {\n  x = (var.a ? var.m : var.n)[\"k\"]\n}\n",
		},
		{
			name:   "element of a literal list within bounds",
			files:  files{"main.tf": "locals {\n  x = element([\"a\", \"b\"], 1)\n}\n"},
			needle: "element",
			title:  `Replace element() with ["a", "b"][1]`,
			edits:  []string{`main.tf 2:7-2:29 "[\"a\", \"b\"][1]"`},
			want:   "locals {\n  x = [\"a\", \"b\"][1]\n}\n",
		},
		{
			name:   "element of a local tuple within bounds",
			files:  files{"main.tf": "locals {\n  zones = [\"a\", \"b\", \"c\"]\n  x     = element(local.zones, 2)\n}\n"},
			needle: "element",
			title:  "Replace element() with local.zones[2]",
			edits:  []string{`main.tf 3:11-3:34 "local.zones[2]"`},
			want:   "locals {\n  zones = [\"a\", \"b\", \"c\"]\n  x     = local.zones[2]\n}\n",
		},
		{
			name:     "element past the end wraps around",
			files:    files{"main.tf": "locals {\n  x = element([\"a\", \"b\"], 2)\n}\n"},
			needle:   "element",
			title:    "Replace element() with an index",
			disabled: "element() wraps around: index 2 is past the end of the list of 2",
		},
		{
			name:     "element of a variable with a literal index",
			files:    files{"main.tf": "locals {\n  x = element(var.zones, 0)\n}\n"},
			needle:   "element",
			title:    "Replace element() with an index",
			disabled: "element() wraps around, and the length of the list is not known statically",
		},
		{
			name: "element with count.index where count is the length of the list",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = length(var.zones)
  filename = element(var.zones, count.index)
}
`},
			needle:   "element",
			varTypes: map[string]cty.Type{"zones": cty.List(cty.String)},
			title:    "Replace element() with var.zones[count.index]",
			edits:    []string{`main.tf 3:14-3:45 "var.zones[count.index]"`},
			want: `resource "local_file" "f" {
  count    = length(var.zones)
  filename = var.zones[count.index]
}
`,
		},
		{
			name: "element with count.index where count is the length of the list or 0",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = var.on && length(var.zones) > 0 ? (length(var.zones)) : 0
  filename = element(var.zones, count.index)
}
`},
			needle:   "element",
			varTypes: map[string]cty.Type{"zones": cty.Tuple([]cty.Type{cty.String})},
			title:    "Replace element() with var.zones[count.index]",
			edits:    []string{`main.tf 3:14-3:45 "var.zones[count.index]"`},
			want: `resource "local_file" "f" {
  count    = var.on && length(var.zones) > 0 ? (length(var.zones)) : 0
  filename = var.zones[count.index]
}
`,
		},
		{
			name: "element with count.index where count may exceed the length",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = length(var.zones) > 0 ? length(var.zones) : 1
  filename = element(var.zones, count.index)
}
`},
			needle:   "element",
			varTypes: map[string]cty.Type{"zones": cty.List(cty.String)},
			title:    "Replace element() with an index",
			disabled: "element() wraps around, and count is not length() of the same list",
		},
		{
			name: "element with count.index of a set",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = length(var.zones)
  filename = element(var.zones, count.index)
}
`},
			needle:   "element",
			varTypes: map[string]cty.Type{"zones": cty.Set(cty.String)},
			title:    "Replace element() with an index",
			disabled: "the collection is not known to be a list, which an index needs",
		},
		{
			name: "element with count.index of another list",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = length(var.a)
  filename = element(var.b, count.index)
}
`},
			needle:   "element",
			varTypes: map[string]cty.Type{"a": cty.List(cty.String), "b": cty.List(cty.String)},
			title:    "Replace element() with an index",
			disabled: "element() wraps around, and count is not length() of the same list",
		},
		{
			name: "element with count.index of a splat",
			files: files{"main.tf": `resource "local_file" "f" {
  count    = length(local_file.src[*].id)
  filename = element(local_file.src[*].id, count.index)
}
`},
			needle: "element",
			title:  "Replace element() with local_file.src[*].id[count.index]",
			edits:  []string{`main.tf 3:14-3:56 "local_file.src[*].id[count.index]"`},
			want: `resource "local_file" "f" {
  count    = length(local_file.src[*].id)
  filename = local_file.src[*].id[count.index]
}
`,
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			env := tc.files.newEnv()
			env.VariableType = func(dir, name string) (cty.Type, bool) {
				ty, ok := tc.varTypes[name]
				return ty, ok
			}
			pos := tc.files.rangeOf(t, "main.tf", tc.needle, 1).Start
			pos = posAt([]byte(tc.files["main.tf"]), pos.Byte+tc.offset)
			actions := Intentions(env, tc.files.doc("main.tf"), pos)
			if tc.disabled != "" {
				if len(actions) != 1 || actions[0].Title != tc.title || actions[0].Disabled != tc.disabled || len(actions[0].Edits) != 0 {
					t.Fatalf("expected the disabled action %q (%s), got %#v", tc.title, tc.disabled, actions)
				}
				return
			}
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, files{"main.tf": tc.want})
			if a.Kind != KindRefactorRewrite || a.Disabled != "" {
				t.Errorf("expected an enabled rewrite, got %#v", a)
			}
		})
	}
}

func TestRewriteCall_notOffered(t *testing.T) {
	for i, src := range []string{
		"locals {\n  x = lookup(var.m, \"k\", \"default\")\n}\n",
		"locals {\n  x = element(var.l...)\n}\n",
		"locals {\n  x = upper(\"element\")\n}\n",
	} {
		fsys := files{"main.tf": src}
		pos := fsys.rangeOf(t, "main.tf", "x = ", 1).End
		pos = posAt([]byte(src), pos.Byte+2)
		for _, a := range Intentions(fsys.newEnv(), fsys.doc("main.tf"), pos) {
			if strings.Contains(a.Title, "element") || strings.Contains(a.Title, "lookup") {
				t.Errorf("%d: unexpected action %#v", i, a)
			}
		}
	}
}
