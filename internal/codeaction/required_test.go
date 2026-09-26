// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// bodyRange is the range of the body (braces included) of the block
// starting at the nth occurrence of header.
func bodyRange(t *testing.T, fsys files, name, header string, nth int) hcl.Range {
	t.Helper()
	at := fsys.rangeOf(t, name, header, nth)
	body, ok := parse(at.Filename, []byte(fsys[name]))
	if !ok {
		t.Fatalf("%s does not parse", name)
	}
	var found *hcl.Range
	var walk func(body *hclsyntax.Body)
	walk = func(body *hclsyntax.Body) {
		for _, block := range body.Blocks {
			if block.Range().Start.Byte == at.Start.Byte {
				rng := block.Body.SrcRange
				found = &rng
			}
			walk(block.Body)
		}
	}
	walk(body)
	if found == nil {
		t.Fatalf("no block at %q in %s", header, name)
	}
	return *found
}

func TestAddRequiredArguments(t *testing.T) {
	testCases := []struct {
		name   string
		src    string
		header string
		data   map[string]interface{}
		schema bool
		title  string
		edits  []string
		want   string
	}{
		{
			name: "every type, aligned with the existing arguments, and a required block",
			src: `resource "local_file" "f" {
  content = "x"
}
`,
			header: `resource "local_file"`,
			schema: true,
			title:  "Add required arguments: anything, filename, lines and 4 more",
			edits:  []string{`main.tf 2:1-3:1 "  content     = \"x\"\n  anything    = null\n  filename    = \"\"\n  lines       = []\n  permissions = 1\n  sensitive   = false\n  tags        = {}\n\n  rule {\n    port = 1\n  }\n"`},
			want: `resource "local_file" "f" {
  content     = "x"
  anything    = null
  filename    = ""
  lines       = []
  permissions = 1
  sensitive   = false
  tags        = {}

  rule {
    port = 1
  }
}
`,
		},
		{
			name:   "an empty body on one line",
			src:    "module \"c\" {source = \"./child\"}\n",
			header: `module "c"`,
			schema: true,
			title:  `Add required argument "name"`,
			edits:  []string{`main.tf 1:1-2:1 "module \"c\" {\n  source = \"./child\"\n  name   = \"\"\n}\n"`},
			want:   "module \"c\" {\n  source = \"./child\"\n  name   = \"\"\n}\n",
		},
		{
			name:   "a nested block that is not formatted keeps its layout",
			src:    "resource \"local_file\" \"f\" {\n  filename=\"a\"\n  permissions = 1\n  sensitive = true\n  lines = []\n  tags = {}\n  anything = 1\n  rule {\n  }\n}\n",
			header: "rule",
			schema: true,
			title:  `Add required argument "port"`,
			edits:  []string{`main.tf 9:1-9:1 "    port = 1\n"`},
			want:   "resource \"local_file\" \"f\" {\n  filename=\"a\"\n  permissions = 1\n  sensitive = true\n  lines = []\n  tags = {}\n  anything = 1\n  rule {\n    port = 1\n  }\n}\n",
		},
		{
			name:   "the value of an output",
			src:    "output \"o\" {}\n",
			header: `output "o"`,
			schema: true,
			title:  `Add required argument "value"`,
			edits:  []string{`main.tf 1:1-2:1 "output \"o\" {\n  value = null\n}\n"`},
			want:   "output \"o\" {\n  value = null\n}\n",
		},
		{
			name:   "without a schema, the names from the data",
			src:    "resource \"x_y\" \"z\" {\n  a = 1\n}\n",
			header: `resource "x_y"`,
			data:   map[string]interface{}{"attributes": []interface{}{"b"}, "blocks": []interface{}{"c"}},
			title:  "Add required arguments: b, c",
			edits:  []string{`main.tf 3:1-3:1 "  b = null\n\n  c {\n  }\n"`},
			want:   "resource \"x_y\" \"z\" {\n  a = 1\n  b = null\n\n  c {\n  }\n}\n",
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			fsys := files{"main.tf": tc.src}
			env := fsys.newEnv()
			if tc.schema {
				env = schemaEnv(fsys)
			}
			diag := Diagnostic{Code: CodeMissingRequiredAttribute, Data: tc.data, Range: bodyRange(t, fsys, "main.tf", tc.header, 1)}
			actions := QuickFixes(env, fsys.doc("main.tf"), diag)
			a := expectAction(t, fsys, actions, tc.title, tc.edits, files{"main.tf": tc.want})
			if !a.Preferred {
				t.Errorf("expected a preferred fix")
			}
		})
	}
}

func TestAddRequiredArguments_onlyReferencesMissing(t *testing.T) {
	fsys := files{"main.tf": "moved {\n}\n"}
	diag := Diagnostic{Code: CodeMissingRequiredAttribute, Range: bodyRange(t, fsys, "main.tf", "moved", 1)}
	if actions := QuickFixes(schemaEnv(fsys), fsys.doc("main.tf"), diag); len(actions) != 0 {
		t.Fatalf("an address has no placeholder, expected no fix, got %#v", actions)
	}
}
