// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestShortType(t *testing.T) {
	long := "object({\n"
	for i := 0; i < 20; i++ {
		long += fmt.Sprintf("  attr_%02d = optional(string)\n", i)
	}
	long += "})"

	testCases := []struct {
		name string
		src  string
		want string
	}{
		{
			"short and shallow: as written, comments included",
			`object({
  # the name
  name  = string
  ports = list(number)
})`,
			`object({
  # the name
  name  = string
  ports = list(number)
})`,
		},
		{
			"objects deeper than two levels are summarized",
			`map(object({
  name = string
  disk = optional(object({
    size = number
    encryption = optional(object({
      kms_key = string
      enabled = bool
    }))
  }), {})
}))`,
			`map(object({
  name = string
  disk = optional(object({
    size       = number
    encryption = optional(object({ … 2 attributes }))
  }), {})
}))`,
		},
		{
			"too many lines: the attributes that do not fit are counted",
			long,
			`object({
  attr_00 = optional(string)
  attr_01 = optional(string)
  attr_02 = optional(string)
  attr_03 = optional(string)
  attr_04 = optional(string)
  attr_05 = optional(string)
  attr_06 = optional(string)
  attr_07 = optional(string)
  attr_08 = optional(string)
  # … 11 more attributes (Go to Definition shows the full type)
})`,
		},
		{
			"a nested object that does not fit is counted whole",
			`list(object({
  a = string
  b = string
  c = string
  d = string
  e = string
  f = string
  g = string
  h = string
  i = string
  nested = object({
    x = string
    y = string
    z = string
  })
}))`,
			`list(object({
  a = string
  b = string
  c = string
  d = string
  e = string
  f = string
  g = string
  h = string
  i = string
  # … 1 more attribute (Go to Definition shows the full type)
}))`,
		},
		{
			"tuples and defaults stay on one line",
			`tuple([string, object({
  a = optional(list(string), ["x", "y"])
  b = object({ c = object({ d = string }) })
})])`,
			`tuple([string, object({
  a = optional(list(string), ["x", "y"])
  b = object({
    c = object({ … 1 attribute })
  })
})])`,
		},
		{
			"not a valid expression: as written",
			"object({\n  a = \n",
			"object({\n  a = \n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortType(tc.src)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected type (-want +got):\n%s", diff)
			}
			if n := strings.Count(got, "\n") + 1; n > maxTypeLines && got != tc.src {
				t.Errorf("summary has %d lines, more than %d", n, maxTypeLines)
			}
		})
	}
}

func TestShortDescription(t *testing.T) {
	var paras []string
	for i := 0; i < 8; i++ {
		paras = append(paras, fmt.Sprintf("Paragraph %d, line one.\nParagraph %d, line two.", i, i))
	}
	long := strings.Join(paras, "\n\n")
	fenced := "Example:\n```\n" + strings.Repeat("x = 1\n", 30) + "```"

	testCases := []struct {
		name string
		desc string
		want string
	}{
		{"short: unchanged", "One line.", "One line."},
		{
			"long: cut at a paragraph break",
			long,
			strings.Join(paras[:5], "\n\n") + "\n\n_… 9 more lines of description (Go to Definition shows them)_",
		},
		{
			"a code fence left open is closed",
			fenced,
			"Example:\n```\n" + strings.Repeat("x = 1\n", 14) + "```\n\n_… 17 more lines of description (Go to Definition shows them)_",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, shortDescription(tc.desc)); diff != "" {
				t.Errorf("unexpected description (-want +got):\n%s", diff)
			}
		})
	}
}

func TestModuleCallHover_capsLists(t *testing.T) {
	var child strings.Builder
	child.WriteString("variable \"needed\" {}\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&child, "variable \"in_%02d\" {\n  default = %d\n}\n", i, i)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&child, "output \"out_%02d\" {\n  value = %d\n}\n", i, i)
	}
	root := writeTree(t, map[string]string{
		"main.tf": `module "big" {
  source = "./big"
  needed = "x"
  in_29  = 1
}
`,
		"big/main.tf": child.String(),
	})
	ev := treeEvaluator(t, root, nil)
	h, ok := ev.HoverAt("main.tf", posOf(t, ev, "main.tf", `"big"`, 1), treeEnv)
	if !ok {
		t.Fatal("no hover on the module call")
	}
	for _, want := range []string{
		"- `needed` _any_ · required",
		// an input the call sets comes before the defaults
		"- `in_29` _any_ · optional = `1`",
		"- … 19 more inputs\n",
		"- … 8 more outputs",
	} {
		if !strings.Contains(h.Content, want) {
			t.Errorf("hover lacks %q:\n%s", want, h.Content)
		}
	}
	if n := strings.Count(h.Content, "\n") + 1; n > 40 {
		t.Errorf("hover has %d lines:\n%s", n, h.Content)
	}
}
