// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/zclconf/go-cty/cty"
)

// testSchema describes a resource type local_file (filename a string,
// permissions a number, tags a map of strings), module calls of ./child
// (name a string), outputs and moved blocks.
func testSchema() *schema.BodySchema {
	return &schema.BodySchema{
		Blocks: map[string]*schema.BlockSchema{
			"resource": {
				Labels: []*schema.LabelSchema{{Name: "type", IsDepKey: true}, {Name: "name"}},
				Body:   &schema.BodySchema{Extensions: &schema.BodyExtensions{Count: true, ForEach: true}},
				DependentBody: map[schema.SchemaKey]*schema.BodySchema{
					schema.NewSchemaKey(schema.DependencyKeys{Labels: []schema.LabelDependent{{Index: 0, Value: "local_file"}}}): {
						Attributes: map[string]*schema.AttributeSchema{
							"filename":    {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.String}},
							"permissions": {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.Number}},
							"sensitive":   {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.Bool}},
							"lines":       {IsRequired: true, Constraint: schema.OneOf{schema.AnyExpression{OfType: cty.List(cty.String)}, schema.List{Elem: schema.AnyExpression{OfType: cty.String}}}},
							"tags":        {IsRequired: true, Constraint: schema.OneOf{schema.AnyExpression{OfType: cty.Map(cty.String)}, schema.Map{Elem: schema.AnyExpression{OfType: cty.String}}}},
							"anything":    {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.DynamicPseudoType}},
							"content":     {IsOptional: true, Constraint: schema.AnyExpression{OfType: cty.String}},
						},
						Blocks: map[string]*schema.BlockSchema{
							"rule": {
								MinItems: 1,
								Body: &schema.BodySchema{
									Attributes: map[string]*schema.AttributeSchema{
										"port": {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.Number}},
										"note": {IsOptional: true, Constraint: schema.AnyExpression{OfType: cty.String}},
									},
								},
							},
							"extra": {Body: &schema.BodySchema{}},
						},
					},
				},
			},
			"module": {
				Labels: []*schema.LabelSchema{{Name: "name"}},
				Body: &schema.BodySchema{
					Attributes: map[string]*schema.AttributeSchema{
						"source": {IsRequired: true, IsDepKey: true, Constraint: schema.LiteralType{Type: cty.String}},
					},
				},
				DependentBody: map[schema.SchemaKey]*schema.BodySchema{
					schema.NewSchemaKey(schema.DependencyKeys{Attributes: []schema.AttributeDependent{{
						Name: "source", Expr: schema.ExpressionValue{Static: cty.StringVal("./child")},
					}}}): {
						Attributes: map[string]*schema.AttributeSchema{
							"name":  {IsRequired: true, Constraint: schema.OneOf{schema.AnyExpression{OfType: cty.String}}},
							"ports": {IsOptional: true, Constraint: schema.OneOf{schema.AnyExpression{OfType: cty.Set(cty.Number)}}},
						},
					},
				},
			},
			"output": {
				Labels: []*schema.LabelSchema{{Name: "name"}},
				Body: &schema.BodySchema{
					Attributes: map[string]*schema.AttributeSchema{
						"value": {IsRequired: true, Constraint: schema.AnyExpression{OfType: cty.DynamicPseudoType}},
					},
				},
			},
			"moved": {
				Body: &schema.BodySchema{
					Attributes: map[string]*schema.AttributeSchema{
						"from": {IsRequired: true, Constraint: schema.Reference{OfScopeId: "resource"}},
						"to":   {IsRequired: true, Constraint: schema.Reference{OfScopeId: "resource"}},
					},
				},
			},
		},
	}
}

func schemaEnv(fsys files) Env {
	env := fsys.newEnv()
	s := testSchema()
	env.Schema = func(dir string) *schema.BodySchema { return s }
	return env
}

func TestDeclareVariable(t *testing.T) {
	testCases := []struct {
		name   string
		files  files
		doc    string
		needle string
		data   map[string]interface{}
		create bool
		title  string
		edits  []string
		want   files
		// creates is the file the action creates, if any
		creates string
	}{
		{
			name: "typed by the resource argument, in variables.tf",
			files: files{
				"main.tf": `resource "local_file" "f" {
  filename = var.path
}
`,
				"variables.tf": "variable \"other\" {}\n",
			},
			doc:    "main.tf",
			needle: "var.path",
			data:   map[string]interface{}{"address": "var.path"},
			title:  `Declare variable "path" in variables.tf`,
			edits:  []string{`variables.tf 2:1-2:1 "\nvariable \"path\" {\n  type = string\n}\n"`},
			want: files{
				"variables.tf": "variable \"other\" {}\n\nvariable \"path\" {\n  type = string\n}\n",
			},
		},
		{
			name: "count is a number, in the file with the most variables",
			files: files{
				"main.tf": `resource "local_file" "f" {
  count    = var.copies
  filename = "a"
}

variable "one" {}
`,
				"inputs.tf": "variable \"two\" {}\nvariable \"three\" {}",
			},
			doc:    "main.tf",
			needle: "var.copies",
			title:  `Declare variable "copies" in inputs.tf`,
			edits:  []string{`inputs.tf 2:20-2:20 "\n\nvariable \"copies\" {\n  type = number\n}\n"`},
			want: files{
				"inputs.tf": "variable \"two\" {}\nvariable \"three\" {}\n\nvariable \"copies\" {\n  type = number\n}\n",
			},
		},
		{
			name: "a module input's type",
			files: files{
				"main.tf": "module \"c\" {\n  source = \"./child\"\n  name   = var.label\n}\n",
			},
			doc:     "main.tf",
			needle:  "var.label",
			create:  true,
			title:   `Declare variable "label" in a new variables.tf`,
			edits:   []string{`variables.tf 1:1-1:1 "variable \"label\" {\n  type = string\n}\n"`},
			creates: "variables.tf",
			want: files{
				"variables.tf": "variable \"label\" {\n  type = string\n}\n",
			},
		},
		{
			name: "no type inside a template, appended to the document without file creation",
			files: files{
				"main.tf": "resource \"local_file\" \"f\" {\n  filename = \"${var.dir}/a\"\n}\n",
			},
			doc:    "main.tf",
			needle: "var.dir",
			title:  `Declare variable "dir" in main.tf`,
			edits:  []string{`main.tf 4:1-4:1 "\nvariable \"dir\" {}\n"`},
			want: files{
				"main.tf": "resource \"local_file\" \"f\" {\n  filename = \"${var.dir}/a\"\n}\n\nvariable \"dir\" {}\n",
			},
		},
		{
			name: "the address from the range when the data is missing",
			files: files{
				"main.tf":      "locals {\n  x = var.settings.port\n}\n",
				"variables.tf": "",
			},
			doc:    "main.tf",
			needle: "var.settings.port",
			title:  `Declare variable "settings" in variables.tf`,
			edits:  []string{`variables.tf 1:1-1:1 "variable \"settings\" {}\n"`},
			want: files{
				"variables.tf": "variable \"settings\" {}\n",
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			env := schemaEnv(tc.files)
			env.CreateFiles = tc.create
			diag := Diagnostic{Code: CodeUnresolvedReference, Data: tc.data, Range: tc.files.rangeOf(t, tc.doc, tc.needle, 1)}
			actions := QuickFixes(env, tc.files.doc(tc.doc), diag)
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
			if !a.Preferred {
				t.Errorf("expected a preferred fix")
			}
			if tc.creates == "" && len(a.Create) > 0 {
				t.Errorf("unexpected file creation: %v", a.Create)
			}
			if tc.creates != "" && (len(a.Create) != 1 || a.Create[0] != filepath.Join(root, tc.creates)) {
				t.Errorf("expected to create %s, got %v", tc.creates, a.Create)
			}
		})
	}
}

func TestDeclareTfvarsVariable(t *testing.T) {
	testCases := []struct {
		name   string
		tfvars string
		needle string
		data   map[string]interface{}
		decl   string
	}{
		{"a string from the data", "region = \"eu\"\n", "region", map[string]interface{}{"name": "region", "valueType": "string"}, "variable \"region\" {\n  type = string\n}\n"},
		{"a tuple of one type is a list", "zones = [\"a\", \"b\"]\n", "zones", map[string]interface{}{"name": "zones", "valueType": "tuple([string,string])"}, "variable \"zones\" {\n  type = list(string)\n}\n"},
		{"a map, from the value", "tags = {\n  a = \"x\"\n  b = \"y\"\n}\n", "tags", nil, "variable \"tags\" {\n  type = map(string)\n}\n"},
		{"an object of mixed attributes", "svc = { port = 80, name = \"web\" }\n", "svc", nil, "variable \"svc\" {\n  type = object({ name = string, port = number })\n}\n"},
		{"a tuple of mixed types has no type", "mix = [1, \"a\"]\n", "mix", nil, "variable \"mix\" {}\n"},
		{"an empty list has no type", "none = []\n", "none", nil, "variable \"none\" {}\n"},
		{"a reference has no type", "ref = var.other\n", "ref", nil, "variable \"ref\" {}\n"},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			fsys := files{
				"terraform.tfvars": tc.tfvars,
				"variables.tf":     "variable \"a\" {}\n",
			}
			diag := Diagnostic{Code: CodeTfvarsUndeclaredVariable, Data: tc.data, Range: fsys.rangeOf(t, "terraform.tfvars", tc.needle, 1)}
			actions := QuickFixes(fsys.newEnv(), fsys.doc("terraform.tfvars"), diag)
			name := tc.needle
			expectAction(t, fsys, actions, fmt.Sprintf("Declare variable %q in variables.tf", name),
				[]string{fmt.Sprintf("variables.tf 2:1-2:1 %q", "\n"+tc.decl)},
				files{"variables.tf": "variable \"a\" {}\n\n" + tc.decl})
		})
	}
}

func TestDeclareLocal(t *testing.T) {
	testCases := []struct {
		name  string
		files files
		title string
		edits []string
		want  files
	}{
		{
			name: "aligned in the document's first locals block",
			files: files{
				"main.tf": `locals {
  a = 1
}

output "o" {
  value = local.missing
}
`,
			},
			title: `Declare local "missing"`,
			edits: []string{`main.tf 2:1-3:1 "  a       = 1\n  missing = null # TODO\n"`},
			want: files{
				"main.tf": `locals {
  a       = 1
  missing = null # TODO
}

output "o" {
  value = local.missing
}
`,
			},
		},
		{
			name: "in another file's block",
			files: files{
				"main.tf":   "output \"o\" {\n  value = local.missing\n}\n",
				"locals.tf": "locals {\n  a = 1\n\n  b = 2\n}\n",
			},
			title: `Declare local "missing" in locals.tf`,
			edits: []string{`locals.tf 4:1-5:1 "  b       = 2\n  missing = null # TODO\n"`},
			want: files{
				"locals.tf": "locals {\n  a = 1\n\n  b       = 2\n  missing = null # TODO\n}\n",
			},
		},
		{
			name: "a new block at the end of the document",
			files: files{
				"main.tf": "output \"o\" {\n  value = local.missing\n}\n",
			},
			title: `Declare local "missing"`,
			edits: []string{`main.tf 4:1-4:1 "\nlocals {\n  missing = null # TODO\n}\n"`},
			want: files{
				"main.tf": "output \"o\" {\n  value = local.missing\n}\n\nlocals {\n  missing = null # TODO\n}\n",
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			diag := Diagnostic{Code: CodeUnresolvedReference, Data: map[string]interface{}{"address": "local.missing"}, Range: tc.files.rangeOf(t, "main.tf", "local.missing", 1)}
			actions := QuickFixes(tc.files.newEnv(), tc.files.doc("main.tf"), diag)
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
			if a.Preferred {
				t.Errorf("declaring a local with a placeholder value must not be preferred")
			}
		})
	}
}
