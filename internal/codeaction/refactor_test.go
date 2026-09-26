// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// refactorCase is a refactoring offered at a needle of a file and then
// resolved.
type refactorCase struct {
	name  string
	files files
	// doc is the file of the request, main.tf when empty.
	doc    string
	needle string
	// nth is the occurrence of needle, the first when 0.
	nth int
	// cursor is the offset of the cursor in needle; -1 selects needle.
	cursor int
	// auto asks as the client does on its own (the lightbulb), rather
	// than as the user does.
	auto  bool
	title string
	// absent expects the title not to be offered.
	absent bool
	// refused is the reason the action is disabled with, or refused with
	// when it is resolved.
	refused string
	edits   []string
	want    files
	// rename is the position (line:character, zero-based) of the rename
	// command, or "" for none.
	rename string
	env    func(env *Env)
}

func (tc refactorCase) request(t *testing.T) (Env, Document, hcl.Range) {
	t.Helper()
	doc := tc.doc
	if doc == "" {
		doc = "main.tf"
	}
	nth := tc.nth
	if nth == 0 {
		nth = 1
	}
	rng := tc.files.rangeOf(t, doc, tc.needle, nth)
	if tc.cursor >= 0 {
		rng.Start = posAt([]byte(tc.files[doc]), rng.Start.Byte+tc.cursor)
		rng.End = rng.Start
	}
	env := tc.files.newEnv()
	env.RenameCommand = "client.rename"
	env.FileURI = func(path string) string { return "file://" + path }
	if tc.env != nil {
		tc.env(&env)
	}
	return env, tc.files.doc(doc), rng
}

func runRefactorCases(t *testing.T, cases []refactorCase) {
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			env, doc, rng := tc.request(t)
			offers := Refactorings(env, doc, rng, !tc.auto)
			var offer *Action
			titles := make([]string, 0, len(offers))
			for i := range offers {
				titles = append(titles, offers[i].Title)
				if offers[i].Title == tc.title {
					offer = &offers[i]
				}
			}
			if tc.absent {
				if offer != nil {
					t.Fatalf("expected no %q, got %#v", tc.title, *offer)
				}
				return
			}
			if offer == nil {
				t.Fatalf("no %q among %q", tc.title, titles)
			}
			if len(offer.Edits) != 0 || offer.Resolve == nil {
				t.Fatalf("an offer carries no edits, only what resolves it: %#v", *offer)
			}
			if offer.Disabled != "" {
				if offer.Disabled != tc.refused {
					t.Fatalf("disabled:\nexpected %q\ngot      %q", tc.refused, offer.Disabled)
				}
				return
			}
			a, err := Resolve(env, doc, *offer.Resolve)
			if err != nil {
				if err.Error() != tc.refused {
					t.Fatalf("refused:\nexpected %q\ngot      %q", tc.refused, err)
				}
				return
			}
			if tc.refused != "" || tc.edits == nil {
				t.Fatalf("expected a refusal %q, got %#v", tc.refused, a)
			}
			expectAction(t, tc.files, []Action{a}, tc.title, tc.edits, tc.want)
			rename := ""
			if a.Command != nil {
				rename = fmt.Sprint(a.Command.Arguments[1])
				rename = strings.Trim(strings.ReplaceAll(rename, " ", ":"), "{}")
				if a.Command.Name != "client.rename" || a.Command.Arguments[0] != "file://"+doc.Path {
					t.Errorf("unexpected command %#v", a.Command)
				}
			}
			if rename != tc.rename {
				t.Errorf("rename at %q, expected %q", rename, tc.rename)
			}
		})
	}
}

// listValue is a static evaluator which knows one list of strings.
func listValue(elems ...string) func(env *Env) {
	return func(env *Env) {
		env.StaticValue = func(dir string, expr hcl.Expression) StaticValue {
			vals := make([]cty.Value, 0, len(elems))
			for _, e := range elems {
				vals = append(vals, cty.StringVal(e))
			}
			if len(vals) == 0 {
				return StaticValue{Value: cty.ListValEmpty(cty.String), Known: true, Sources: []string{"default"}}
			}
			return StaticValue{Value: cty.ListVal(vals), Known: true, Sources: []string{"default"}}
		}
	}
}

const bucketTF = `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = "private"
  tags = {
    Name = upper(var.project)
  }
}
`

func TestExtractLocal(t *testing.T) {
	runRefactorCases(t, []refactorCase{
		{
			name:   "a selected template, above its block and its comment",
			files:  files{"main.tf": bucketTF},
			needle: `"${var.project}-logs"`, cursor: -1,
			title: "Extract to local",
			edits: []string{
				`main.tf 5:1-5:1 "locals {\n  bucket = \"${var.project}-logs\"\n}\n\n"`,
				`main.tf 7:1-8:1 "  bucket = local.bucket\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

locals {
  bucket = "${var.project}-logs"
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = local.bucket
  acl    = "private"
  tags = {
    Name = upper(var.project)
  }
}
`,
			},
			rename: "10:17",
		},
		{
			name:   "the value of the object item at the cursor, named by its key",
			files:  files{"main.tf": bucketTF + "\nlocals {\n  a = 1\n}\n"},
			needle: "upper", cursor: 2,
			title: "Extract to local",
			edits: []string{
				`main.tf 10:1-11:1 "    Name = local.bucket_name\n"`,
				`main.tf 15:1-16:1 "  a           = 1\n  bucket_name = upper(var.project)\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = "private"
  tags = {
    Name = local.bucket_name
  }
}

locals {
  a           = 1
  bucket_name = upper(var.project)
}
`,
			},
			rename: "9:17",
		},
		{
			name:   "a name taken gets a suffix",
			files:  files{"main.tf": bucketTF, "locals.tf": "locals {\n  bucket_acl = \"public\"\n}\n"},
			needle: `"private"`, cursor: -1, auto: true,
			title: "Extract to local",
			edits: []string{
				`main.tf 5:1-5:1 "locals {\n  bucket_acl_2 = \"private\"\n}\n\n"`,
				`main.tf 8:1-9:1 "  acl    = local.bucket_acl_2\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

locals {
  bucket_acl_2 = "private"
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = local.bucket_acl_2
  tags = {
    Name = upper(var.project)
  }
}
`,
			},
			rename: "11:17",
		},
		{
			name:   "a subexpression of a local value joins its block",
			files:  files{"main.tf": "locals {\n  region = upper(\"eu-north-1\")\n  zone   = \"a\"\n}\n"},
			needle: `"eu-north-1"`, cursor: -1,
			title: "Extract to local",
			edits: []string{
				`main.tf 2:1-4:1 "  region      = upper(local.region_part)\n  zone        = \"a\"\n  region_part = \"eu-north-1\"\n"`,
			},
			want: files{
				"main.tf": `locals {
  region      = upper(local.region_part)
  zone        = "a"
  region_part = "eu-north-1"
}
`,
			},
			rename: "1:28",
		},
		{
			name: "a multi-line value moves with its lines",
			files: files{"main.tf": `resource "terraform_data" "web" {
  input = {
    ports = [
      80,
      443,
    ]
  }
}
`},
			needle: "[\n      80", cursor: 0,
			title: "Extract to local",
			edits: []string{
				`main.tf 1:1-1:1 "locals {\n  web_ports = [\n    80,\n    443,\n  ]\n}\n\n"`,
				`main.tf 3:1-7:1 "    ports = local.web_ports\n"`,
			},
			want: files{
				"main.tf": `locals {
  web_ports = [
    80,
    443,
  ]
}

resource "terraform_data" "web" {
  input = {
    ports = local.web_ports
  }
}
`,
			},
			rename: "9:18",
		},
		{
			name:   "a heredoc keeps its lines",
			files:  files{"main.tf": "output \"script\" {\n  value = <<-EOT\n    echo hi\n  EOT\n}\n"},
			needle: "<<-EOT", cursor: 2,
			title: "Extract to local",
			edits: []string{
				`main.tf 1:1-1:1 "locals {\n  script = <<-EOT\n    echo hi\n  EOT\n}\n\n"`,
				`main.tf 2:1-5:1 "  value = local.script\n"`,
			},
			want: files{
				"main.tf": `locals {
  script = <<-EOT
    echo hi
  EOT
}

output "script" {
  value = local.script
}
`,
			},
			rename: "7:16",
		},
		{
			name:   "inside an interpolation",
			files:  files{"main.tf": "output \"o\" {\n  value = \"${var.a}-${upper(var.b)}\"\n}\n"},
			needle: "upper(var.b)", cursor: -1, auto: true,
			title: "Extract to local",
			edits: []string{
				`main.tf 1:1-1:1 "locals {\n  o = upper(var.b)\n}\n\n"`,
				`main.tf 2:1-3:1 "  value = \"${var.a}-${local.o}\"\n"`,
			},
			want: files{
				"main.tf": `locals {
  o = upper(var.b)
}

output "o" {
  value = "${var.a}-${local.o}"
}
`,
			},
			rename: "5:28",
		},
		{
			name:   "a one-line locals block is spread out",
			files:  files{"main.tf": "locals { a = 1 }\n\noutput \"o\" {\n  value = var.x + 1\n}\n"},
			needle: "var.x + 1", cursor: -1,
			title: "Extract to local",
			edits: []string{
				`main.tf 1:1-2:1 "locals {\n  a = 1\n  o = var.x + 1\n}\n"`,
				`main.tf 4:1-5:1 "  value = local.o\n"`,
			},
			want: files{
				"main.tf": `locals {
  a = 1
  o = var.x + 1
}

output "o" {
  value = local.o
}
`,
			},
			rename: "6:16",
		},
		{
			name:   "CRLF line endings",
			files:  files{"main.tf": "locals {\r\n  a = 1\r\n}\r\n\r\noutput \"o\" {\r\n  value = var.x + 1\r\n}\r\n"},
			needle: "var.x + 1", cursor: -1,
			title: "Extract to local",
			edits: []string{
				`main.tf 3:1-3:1 "  o = var.x + 1\r\n"`,
				`main.tf 6:1-7:1 "  value = local.o\r\n"`,
			},
			want: files{
				"main.tf": "locals {\r\n  a = 1\r\n  o = var.x + 1\r\n}\r\n\r\noutput \"o\" {\r\n  value = local.o\r\n}\r\n",
			},
			rename: "6:16",
		},
		{
			name:   "each is not seen by a local value",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  for_each = toset([\"a\"])\n  input    = \"${each.key}-x\"\n}\n"},
			needle: `"${each.key}-x"`, cursor: -1,
			title:   "Extract to local",
			refused: "the expression refers to each.key, which a local value cannot see",
		},
		{
			name:   "nor is count, in the lightbulb",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = 2\n  input = count.index + 1\n}\n"},
			needle: "count.index + 1", cursor: -1, auto: true,
			title:  "Extract to local",
			absent: true,
		},
		{
			name:   "nor self",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  provisioner \"local-exec\" {\n    command = \"echo ${self.id}\"\n  }\n}\n"},
			needle: `"echo ${self.id}"`, cursor: -1,
			title:   "Extract to local",
			refused: "the expression refers to self.id, which a local value cannot see",
		},
		{
			name:   "nor the variable of an enclosing for expression",
			files:  files{"main.tf": "output \"o\" {\n  value = [for s in var.l : upper(s)]\n}\n"},
			needle: "upper(s)", cursor: -1,
			title:   "Extract to local",
			refused: "the expression refers to s, which a local value cannot see",
		},
		{
			name: "nor a dynamic block's iterator",
			files: files{"main.tf": `resource "aws_security_group" "g" {
  dynamic "ingress" {
    for_each = var.ports
    iterator = port
    content {
      from_port = port.value
    }
  }
}
`},
			needle: "port.value", cursor: -1,
			title:   "Extract to local",
			refused: "the expression refers to port.value, which a local value cannot see",
		},
		{
			name:   "a provisioner's when is a keyword",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  provisioner \"local-exec\" {\n    when    = destroy\n    command = \"echo\"\n  }\n}\n"},
			needle: "destroy", cursor: 1,
			title:   "Extract to local",
			refused: "the when argument takes no references",
		},
		{
			name:   "a meta-argument takes no references",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  depends_on = [terraform_data.u]\n}\n"},
			needle: "[terraform_data.u]", cursor: -1,
			title:   "Extract to local",
			refused: "the depends_on argument takes no references",
		},
		{
			name:   "nor does lifecycle",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  lifecycle {\n    prevent_destroy = true\n  }\n}\n"},
			needle: "true", cursor: -1,
			title:   "Extract to local",
			refused: "lifecycle arguments take only literal values",
		},
		{
			name:   "a variable block cannot refer to locals",
			files:  files{"main.tf": "variable \"v\" {\n  default = upper(\"a\")\n}\n"},
			needle: `upper("a")`, cursor: -1,
			title:   "Extract to local",
			refused: "a variable block cannot refer to it",
		},
		{
			name:   "the text of a template is not an expression",
			files:  files{"main.tf": bucketTF},
			needle: "-logs", cursor: -1,
			title:  "Extract to local",
			absent: true,
		},
		{
			name:   "a local value's whole value is one already",
			files:  files{"main.tf": "locals {\n  a = upper(\"x\")\n}\n"},
			needle: "upper", cursor: 1,
			title:  "Extract to local",
			absent: true,
		},
		{
			name:   "a cursor alone is no request",
			files:  files{"main.tf": bucketTF},
			needle: "upper", cursor: 1, auto: true,
			title:  "Extract to local",
			absent: true,
		},
	})
}

func TestIntroduceVariable(t *testing.T) {
	create := func(env *Env) { env.CreateFiles = true }
	runRefactorCases(t, []refactorCase{
		{
			name:   "a string, declared with the other variables",
			files:  files{"main.tf": bucketTF},
			needle: `"private"`, cursor: 1,
			title: "Introduce variable",
			edits: []string{
				`main.tf 13:1-13:1 "\nvariable \"bucket_acl\" {\n  type    = string\n  default = \"private\"\n}\n"`,
				`main.tf 8:1-9:1 "  acl    = var.bucket_acl\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = var.bucket_acl
  tags = {
    Name = upper(var.project)
  }
}

variable "bucket_acl" {
  type    = string
  default = "private"
}
`,
			},
			rename: "7:15",
		},
		{
			name:   "a number in a call, in variables.tf",
			files:  files{"main.tf": "locals {\n  subnet = cidrsubnet(var.cidr, 8, 1)\n}\n", "variables.tf": "variable \"cidr\" {}\n"},
			needle: "8", cursor: -1, auto: true,
			title: "Introduce variable",
			edits: []string{
				`main.tf 2:1-3:1 "  subnet = cidrsubnet(var.cidr, var.subnet_part, 1)\n"`,
				`variables.tf 2:1-2:1 "\nvariable \"subnet_part\" {\n  type    = number\n  default = 8\n}\n"`,
			},
			want: files{
				"main.tf": `locals {
  subnet = cidrsubnet(var.cidr, var.subnet_part, 1)
}
`,
				"variables.tf": `variable "cidr" {}

variable "subnet_part" {
  type    = number
  default = 8
}
`,
			},
			rename: "1:36",
		},
		{
			name:   "a list at the cursor keeps its type (a tuple), in a new variables.tf",
			files:  files{"main.tf": "resource \"terraform_data\" \"z\" {\n  input = {\n    zones = [\n      \"a\",\n      \"b\",\n    ]\n  }\n}\n"},
			needle: `"b"`, cursor: 1, env: create,
			title: "Introduce variable",
			edits: []string{
				`main.tf 3:1-7:1 "    zones = var.z_zones\n"`,
				`variables.tf 1:1-1:1 "variable \"z_zones\" {\n  type = tuple([string, string])\n  default = [\n    \"a\",\n    \"b\",\n  ]\n}\n"`,
			},
			want: files{
				"main.tf": `resource "terraform_data" "z" {
  input = {
    zones = var.z_zones
  }
}
`,
				"variables.tf": `variable "z_zones" {
  type = tuple([string, string])
  default = [
    "a",
    "b",
  ]
}
`,
			},
			rename: "2:16",
		},
		{
			name:   "an object keeps its type where the argument takes any",
			files:  files{"main.tf": bucketTF + "\noutput \"tags\" {\n  value = { Team = \"ops\", Tier = \"web\" }\n}\n"},
			needle: "{ Team", cursor: 0,
			title: "Introduce variable",
			edits: []string{
				`main.tf 15:1-16:1 "  value = var.tags\n"`,
				`main.tf 17:1-17:1 "\nvariable \"tags\" {\n  type    = object({ Team = string, Tier = string })\n  default = { Team = \"ops\", Tier = \"web\" }\n}\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = "private"
  tags = {
    Name = upper(var.project)
  }
}

output "tags" {
  value = var.tags
}

variable "tags" {
  type    = object({ Team = string, Tier = string })
  default = { Team = "ops", Tier = "web" }
}
`,
			},
			rename: "14:14",
		},
		{
			name:   "set in a new terraform.tfvars",
			files:  files{"main.tf": bucketTF},
			needle: `"private"`, cursor: -1, env: create,
			title: "Introduce variable set in terraform.tfvars",
			edits: []string{
				`main.tf 13:1-13:1 "\nvariable \"bucket_acl\" {\n  type = string\n}\n"`,
				`main.tf 8:1-9:1 "  acl    = var.bucket_acl\n"`,
				`terraform.tfvars 1:1-1:1 "bucket_acl = \"private\"\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = var.bucket_acl
  tags = {
    Name = upper(var.project)
  }
}

variable "bucket_acl" {
  type = string
}
`,
				"terraform.tfvars": `bucket_acl = "private"
`,
			},
			rename: "7:15",
		},
		{
			name:   "set in terraform.tfvars, a name it uses taken",
			files:  files{"main.tf": bucketTF, "terraform.tfvars": "project = \"p\"\nbucket_acl = \"public\""},
			needle: `"private"`, cursor: -1,
			title: "Introduce variable set in terraform.tfvars",
			edits: []string{
				`main.tf 13:1-13:1 "\nvariable \"bucket_acl_2\" {\n  type = string\n}\n"`,
				`main.tf 8:1-9:1 "  acl    = var.bucket_acl_2\n"`,
				`terraform.tfvars 2:22-2:22 "\nbucket_acl_2 = \"private\"\n"`,
			},
			want: files{
				"main.tf": `variable "project" {
  type = string
}

# The log bucket.
resource "aws_s3_bucket" "bucket" {
  bucket = "${var.project}-logs"
  acl    = var.bucket_acl_2
  tags = {
    Name = upper(var.project)
  }
}

variable "bucket_acl_2" {
  type = string
}
`,
				"terraform.tfvars": `project = "p"
bucket_acl = "public"
bucket_acl_2 = "private"
`,
			},
			rename: "7:15",
		},
		{
			name:   "not in terraform.tfvars of a called module",
			files:  files{"main.tf": bucketTF},
			needle: `"private"`, cursor: -1,
			env: func(env *Env) {
				env.CreateFiles = true
				env.ModuleCallers = func(dir string) ([]ModuleCaller, bool) {
					return []ModuleCaller{{Dir: "/", Name: "logs"}}, true
				}
			},
			title:   "Introduce variable set in terraform.tfvars",
			refused: "module.logs (in ..) calls this module, and terraform.tfvars sets only the variables of a root module",
		},
		{
			name:   "null has no type",
			files:  files{"main.tf": "output \"o\" {\n  value = null\n}\n"},
			needle: "null", cursor: -1,
			title:   "Introduce variable",
			refused: "null has no type to declare",
		},
		{
			name:   "nor has a list holding null",
			files:  files{"main.tf": "output \"o\" {\n  value = [\"a\", null]\n}\n"},
			needle: `["a", null]`, cursor: -1,
			title:   "Introduce variable",
			refused: "the value holds null, which has no type to declare",
		},
		{
			name:   "an argument of a known type declares that type",
			files:  files{"main.tf": "resource \"local_file\" \"f\" {\n  tags  = { a = \"x\" }\n  lines = []\n}\n"},
			needle: "{ a", cursor: 0, env: func(env *Env) {
				env.Schema = func(dir string) *schema.BodySchema { return testSchema() }
			},
			title: "Introduce variable",
			edits: []string{
				`main.tf 2:1-3:1 "  tags  = var.f_tags\n"`,
				`main.tf 5:1-5:1 "\nvariable \"f_tags\" {\n  type    = map(string)\n  default = { a = \"x\" }\n}\n"`,
			},
			want:   files{"main.tf": "resource \"local_file\" \"f\" {\n  tags  = var.f_tags\n  lines = []\n}\n\nvariable \"f_tags\" {\n  type    = map(string)\n  default = { a = \"x\" }\n}\n"},
			rename: "1:14",
		},
		{
			name:   "even an empty list",
			files:  files{"main.tf": "resource \"local_file\" \"f\" {\n  tags  = { a = \"x\" }\n  lines = []\n}\n"},
			needle: "[]", cursor: -1, env: func(env *Env) {
				env.Schema = func(dir string) *schema.BodySchema { return testSchema() }
			},
			title: "Introduce variable",
			edits: []string{
				`main.tf 3:1-4:1 "  lines = var.f_lines\n"`,
				`main.tf 5:1-5:1 "\nvariable \"f_lines\" {\n  type    = list(string)\n  default = []\n}\n"`,
			},
			want:   files{"main.tf": "resource \"local_file\" \"f\" {\n  tags  = { a = \"x\" }\n  lines = var.f_lines\n}\n\nvariable \"f_lines\" {\n  type    = list(string)\n  default = []\n}\n"},
			rename: "2:14",
		},
		{
			name:   "a variable's default takes no variables",
			files:  files{"main.tf": bucketTF + "\nvariable \"x\" {\n  default = \"a\"\n}\n"},
			needle: `"a"`, cursor: -1,
			title:   "Introduce variable",
			refused: "a variable block cannot refer to it",
		},
		{
			name:   "nor does an output's description",
			files:  files{"main.tf": "output \"o\" {\n  description = \"The id.\"\n  value       = 1\n}\n"},
			needle: `"The id."`, cursor: -1,
			title:   "Introduce variable",
			refused: "the description argument takes no references",
		},
		{
			name:   "nor a provider's alias",
			files:  files{"main.tf": "provider \"aws\" {\n  alias  = \"east\"\n  region = \"us-east-1\"\n}\n"},
			needle: `"east"`, cursor: -1,
			title:   "Introduce variable",
			refused: "the alias argument takes no references",
		},
		{
			name:   "the callers indexed so far disable terraform.tfvars",
			files:  files{"main.tf": bucketTF},
			needle: `"private"`, cursor: -1,
			env: func(env *Env) {
				env.CreateFiles = true
				env.IndexedCallers = func(dir string) []ModuleCaller {
					return []ModuleCaller{{Dir: "/examples/basic", Name: "logs"}}
				}
			},
			title:   "Introduce variable set in terraform.tfvars",
			refused: "module.logs (in ../examples/basic) calls this module, and terraform.tfvars sets only the variables of a root module",
		},
		{
			name:   "nor does a module source",
			files:  files{"main.tf": "module \"m\" {\n  source = \"./m\"\n}\n"},
			needle: `"./m"`, cursor: -1,
			title:   "Introduce variable",
			refused: "the source argument takes no references",
		},
		{
			name:   "a template with interpolation is no literal",
			files:  files{"main.tf": bucketTF},
			needle: `"${var.project}-logs"`, cursor: -1,
			title:  "Introduce variable",
			absent: true,
		},
	})
}

const inlineTF = `locals {
  n    = var.a + 1
  tmpl = "p-${var.b}"
  cfg = {
    size = 3
    tags = ["x", "y"]
  }
  list = concat(var.l, ["z"])
}

output "o" {
  value = {
    doubled = local.n * 2
    text    = "${local.tmpl}/q"
    size    = local.cfg.size
    first   = local.cfg.tags[0]
    label   = "n=${local.n}"
    ids     = local.list[*]
    neg     = -local.n
  }
}
`

func TestInlineLocal(t *testing.T) {
	runRefactorCases(t, []refactorCase{
		{
			name:   "every use, from the declaration",
			files:  files{"main.tf": inlineTF},
			needle: "n    =", cursor: 0,
			title: `Inline local "n"`,
			edits: []string{
				`main.tf 13:1-20:1 "    doubled = (var.a + 1) * 2\n    text    = \"${local.tmpl}/q\"\n    size    = local.cfg.size\n    first   = local.cfg.tags[0]\n    label   = \"n=${var.a + 1}\"\n    ids     = local.list[*]\n    neg     = -(var.a + 1)\n"`,
				`main.tf 2:1-3:1 ""`,
			},
			want: files{
				"main.tf": `locals {
  tmpl = "p-${var.b}"
  cfg = {
    size = 3
    tags = ["x", "y"]
  }
  list = concat(var.l, ["z"])
}

output "o" {
  value = {
    doubled = (var.a + 1) * 2
    text    = "${local.tmpl}/q"
    size    = local.cfg.size
    first   = local.cfg.tags[0]
    label   = "n=${var.a + 1}"
    ids     = local.list[*]
    neg     = -(var.a + 1)
  }
}
`,
			},
		},
		{
			name:   "a template joins the template it is used in",
			files:  files{"main.tf": inlineTF},
			needle: "local.tmpl", cursor: 7,
			title: `Inline local "tmpl"`,
			edits: []string{
				`main.tf 14:1-15:1 "    text    = \"p-${var.b}/q\"\n"`,
				`main.tf 2:1-4:1 "  n = var.a + 1\n"`,
			},
			want: files{
				"main.tf": `locals {
  n = var.a + 1
  cfg = {
    size = 3
    tags = ["x", "y"]
  }
  list = concat(var.l, ["z"])
}

output "o" {
  value = {
    doubled = local.n * 2
    text    = "p-${var.b}/q"
    size    = local.cfg.size
    first   = local.cfg.tags[0]
    label   = "n=${local.n}"
    ids     = local.list[*]
    neg     = -local.n
  }
}
`,
			},
		},
		{
			name:   "one use of a long value, parenthesized for its steps",
			files:  files{"main.tf": inlineTF},
			needle: "local.cfg.size", cursor: 7,
			title: `Inline this use of local "cfg"`,
			edits: []string{
				`main.tf 15:1-20:1 "    size = ({\n      size = 3\n      tags = [\"x\", \"y\"]\n    }).size\n    first = local.cfg.tags[0]\n    label = \"n=${local.n}\"\n    ids   = local.list[*]\n    neg   = -local.n\n"`,
			},
			want: files{
				"main.tf": `locals {
  n    = var.a + 1
  tmpl = "p-${var.b}"
  cfg = {
    size = 3
    tags = ["x", "y"]
  }
  list = concat(var.l, ["z"])
}

output "o" {
  value = {
    doubled = local.n * 2
    text    = "${local.tmpl}/q"
    size = ({
      size = 3
      tags = ["x", "y"]
    }).size
    first = local.cfg.tags[0]
    label = "n=${local.n}"
    ids   = local.list[*]
    neg   = -local.n
  }
}
`,
			},
		},
		{
			name:   "a long value used twice stays",
			files:  files{"main.tf": inlineTF},
			needle: "local.cfg.size", cursor: 7,
			title:   `Inline local "cfg"`,
			refused: "local.cfg is 4 lines long and used twice; inline its uses one at a time",
		},
		{
			name:   "a call needs no parentheses for a splat, and the last local drops its block",
			files:  files{"main.tf": "locals {\n  list = concat(var.l, [\"z\"])\n}\n\noutput \"o\" {\n  value = local.list[*]\n}\n"},
			needle: "local.list", cursor: 7,
			title: `Inline local "list"`,
			edits: []string{
				`main.tf 1:1-5:1 ""`,
				`main.tf 6:1-7:1 "  value = concat(var.l, [\"z\"])[*]\n"`,
			},
			want: files{
				"main.tf": `output "o" {
  value = concat(var.l, ["z"])[*]
}
`,
			},
		},
		{
			name:   "in another file",
			files:  files{"main.tf": "locals {\n  a = 1\n  b = 2\n}\n", "outputs.tf": "output \"a\" {\n  value = local.a\n}\n"},
			needle: "a = 1", cursor: 0,
			title: `Inline local "a"`,
			edits: []string{
				`main.tf 2:1-3:1 ""`,
				`outputs.tf 2:1-3:1 "  value = 1\n"`,
			},
			want: files{
				"main.tf": `locals {
  b = 2
}
`,
				"outputs.tf": `output "a" {
  value = 1
}
`,
			},
		},
		{
			name:   "a template ending with $ before a brace stays an interpolation",
			files:  files{"main.tf": "locals {\n  cur = \"US$\"\n}\n\noutput \"o\" {\n  value = \"${local.cur}{5}\"\n}\n"},
			needle: "local.cur", cursor: 7,
			title: `Inline local "cur"`,
			edits: []string{
				`main.tf 1:1-5:1 ""`,
				`main.tf 6:1-7:1 "  value = \"${\"US$\"}{5}\"\n"`,
			},
			want: files{"main.tf": "output \"o\" {\n  value = \"${\"US$\"}{5}\"\n}\n"},
		},
		{
			name:   "a name the use's surroundings bind",
			files:  files{"main.tf": "locals {\n  x = var.l\n}\n\noutput \"o\" {\n  value = [for var in [1] : local.x]\n}\n"},
			needle: "x = var", cursor: 0,
			title:   `Inline local "x"`,
			refused: "at main.tf:6, the name var is bound by the surrounding expression",
		},
		{
			name:   "a heredoc only replaces a whole argument",
			files:  files{"main.tf": "locals {\n  s = <<EOT\nhi\nEOT\n}\n\noutput \"o\" {\n  value = \"${local.s}!\"\n}\n"},
			needle: "local.s", cursor: 7,
			title:   `Inline this use of local "s"`,
			refused: "local.s is a heredoc, which can replace only a whole argument (not at main.tf:8)",
		},
		{
			name:   "an override declares it again",
			files:  files{"main.tf": "locals {\n  a = 1\n}\n", "main_override.tf": "locals {\n  a = 2\n}\n"},
			needle: "a = 1", cursor: 0,
			title:   `Inline local "a"`,
			refused: "local.a is declared more than once (main.tf, main_override.tf)",
		},
		{
			name:   "a test refers to it",
			files:  files{"main.tf": "locals {\n  a = 1\n}\n", "tests/a.tftest.hcl": "run \"r\" {\n  assert {\n    condition     = local.a == 1\n    error_message = \"a\"\n  }\n}\n"},
			needle: "a = 1", cursor: 0,
			title:   `Inline local "a"`,
			refused: "a test file refers to local.a (tests/a.tftest.hcl:3)",
		},
		{
			name:   "provider = local.x names a provider",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  provider = local.x\n}\n"},
			needle: "local.x", cursor: 7,
			title:  `Inline local "x"`,
			absent: true,
		},
	})
}

const countTF = `resource "terraform_data" "t" {
  count = length(var.names)
  input = "${var.names[count.index]}-x"

  triggers_replace = element(var.names, count.index)
}

resource "terraform_data" "after" {
  input = terraform_data.t[0].output
}

output "ids" {
  value = terraform_data.t[*].id
}

output "n" {
  value = length(terraform_data.t)
}

output "names" {
  depends_on = [terraform_data.t]
  value      = var.names
}
`

func TestCountToForEach(t *testing.T) {
	runRefactorCases(t, []refactorCase{
		{
			name:   "count = length(list)",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 1, env: listValue("a", "b"),
			title: "Convert count to for_each with 2 moved blocks",
			edits: []string{
				`main.tf 13:1-14:1 "  value = values(terraform_data.t)[*].id\n"`,
				`main.tf 24:1-24:1 "\nmoved {\n  from = terraform_data.t[0]\n  to   = terraform_data.t[\"a\"]\n}\n\nmoved {\n  from = terraform_data.t[1]\n  to   = terraform_data.t[\"b\"]\n}\n"`,
				`main.tf 2:1-6:1 "  for_each = toset(var.names)\n  input    = \"${each.value}-x\"\n\n  triggers_replace = each.value\n"`,
				`main.tf 9:1-10:1 "  input = terraform_data.t[\"a\"].output\n"`,
			},
			want: files{
				"main.tf": `resource "terraform_data" "t" {
  for_each = toset(var.names)
  input    = "${each.value}-x"

  triggers_replace = each.value
}

resource "terraform_data" "after" {
  input = terraform_data.t["a"].output
}

output "ids" {
  value = values(terraform_data.t)[*].id
}

output "n" {
  value = length(terraform_data.t)
}

output "names" {
  depends_on = [terraform_data.t]
  value      = var.names
}

moved {
  from = terraform_data.t[0]
  to   = terraform_data.t["a"]
}

moved {
  from = terraform_data.t[1]
  to   = terraform_data.t["b"]
}
`,
			},
		},
		{
			name:   "from the block's header, with keys from terraform.tfvars",
			files:  files{"main.tf": countTF},
			needle: `"t" {`, cursor: 1,
			env: func(env *Env) {
				listValue("a")(env)
				sv := env.StaticValue
				env.StaticValue = func(dir string, expr hcl.Expression) StaticValue {
					v := sv(dir, expr)
					v.Sources = []string{"terraform.tfvars"}
					return v
				}
			},
			title: "Convert count to for_each with 1 moved block (keys from terraform.tfvars)",
			edits: []string{
				`main.tf 13:1-14:1 "  value = values(terraform_data.t)[*].id\n"`,
				`main.tf 24:1-24:1 "\nmoved {\n  from = terraform_data.t[0]\n  to   = terraform_data.t[\"a\"]\n}\n"`,
				`main.tf 2:1-6:1 "  for_each = toset(var.names)\n  input    = \"${each.value}-x\"\n\n  triggers_replace = each.value\n"`,
				`main.tf 9:1-10:1 "  input = terraform_data.t[\"a\"].output\n"`,
			},
			want: files{
				"main.tf": `resource "terraform_data" "t" {
  for_each = toset(var.names)
  input    = "${each.value}-x"

  triggers_replace = each.value
}

resource "terraform_data" "after" {
  input = terraform_data.t["a"].output
}

output "ids" {
  value = values(terraform_data.t)[*].id
}

output "n" {
  value = length(terraform_data.t)
}

output "names" {
  depends_on = [terraform_data.t]
  value      = var.names
}

moved {
  from = terraform_data.t[0]
  to   = terraform_data.t["a"]
}
`,
			},
		},
		{
			name:   "a data source moves nothing",
			files:  files{"main.tf": "data \"local_file\" \"f\" {\n  count    = length(local.paths)\n  filename = local.paths[count.index]\n}\n\noutput \"o\" {\n  value = data.local_file.f[0].content\n}\n"},
			needle: "count", cursor: 0, env: listValue("x.txt"),
			title: "Convert count to for_each",
			edits: []string{
				`main.tf 2:1-4:1 "  for_each = toset(local.paths)\n  filename = each.value\n"`,
				`main.tf 7:1-8:1 "  value = data.local_file.f[\"x.txt\"].content\n"`,
			},
			want: files{
				"main.tf": `data "local_file" "f" {
  for_each = toset(local.paths)
  filename = each.value
}

output "o" {
  value = data.local_file.f["x.txt"].content
}
`,
			},
		},
		{
			name:   "a module call",
			files:  files{"main.tf": "module \"m\" {\n  source = \"./m\"\n  count  = length(var.zones)\n  zone   = var.zones[count.index]\n}\n\noutput \"o\" {\n  value = module.m[1].zone\n}\n"},
			needle: "count", cursor: 0, env: listValue("a", "b"),
			title: "Convert count to for_each with 2 moved blocks",
			edits: []string{
				`main.tf 10:1-10:1 "\nmoved {\n  from = module.m[0]\n  to   = module.m[\"a\"]\n}\n\nmoved {\n  from = module.m[1]\n  to   = module.m[\"b\"]\n}\n"`,
				`main.tf 2:1-5:1 "  source   = \"./m\"\n  for_each = toset(var.zones)\n  zone     = each.value\n"`,
				`main.tf 8:1-9:1 "  value = module.m[\"b\"].zone\n"`,
			},
			want: files{
				"main.tf": `module "m" {
  source   = "./m"
  for_each = toset(var.zones)
  zone     = each.value
}

output "o" {
  value = module.m["b"].zone
}

moved {
  from = module.m[0]
  to   = module.m["a"]
}

moved {
  from = module.m[1]
  to   = module.m["b"]
}
`,
			},
		},
		{
			name:   "a toggle",
			files:  files{"main.tf": "resource \"random_id\" \"s\" {\n  count       = var.on ? 1 : 0\n  byte_length = 4 + count.index\n}\n\noutput \"o\" {\n  value = one(random_id.s[*].hex)\n}\n"},
			needle: "count", cursor: 0,
			title: `Convert count to for_each keyed "this"`,
			edits: []string{
				`main.tf 2:1-4:1 "  for_each    = var.on ? toset([\"this\"]) : toset([])\n  byte_length = 4 + 0\n"`,
				`main.tf 7:1-8:1 "  value = one(values(random_id.s)[*].hex)\n"`,
				`main.tf 9:1-9:1 "\nmoved {\n  from = random_id.s[0]\n  to   = random_id.s[\"this\"]\n}\n"`,
			},
			want: files{
				"main.tf": `resource "random_id" "s" {
  for_each    = var.on ? toset(["this"]) : toset([])
  byte_length = 4 + 0
}

output "o" {
  value = one(values(random_id.s)[*].hex)
}

moved {
  from = random_id.s[0]
  to   = random_id.s["this"]
}
`,
			},
		},
		{
			name:   "an inverted toggle",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = (var.off ? 0 : 1)\n}\n"},
			needle: "count", cursor: 0,
			title: `Convert count to for_each keyed "this"`,
			edits: []string{
				`main.tf 2:1-3:1 "  for_each = var.off ? toset([]) : toset([\"this\"])\n"`,
				`main.tf 4:1-4:1 "\nmoved {\n  from = terraform_data.t[0]\n  to   = terraform_data.t[\"this\"]\n}\n"`,
			},
			want: files{
				"main.tf": `resource "terraform_data" "t" {
  for_each = var.off ? toset([]) : toset(["this"])
}

moved {
  from = terraform_data.t[0]
  to   = terraform_data.t["this"]
}
`,
			},
		},
		{
			name:   "an unknown list",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0,
			env: func(env *Env) {
				env.StaticValue = func(dir string, expr hcl.Expression) StaticValue {
					return StaticValue{Value: cty.DynamicVal, Reason: "var.names has no value"}
				}
			},
			title:   "Convert count to for_each",
			refused: "the value of var.names is not known statically (var.names has no value), and the moved blocks need its keys",
		},
		{
			name:   "no static evaluator",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0,
			title:   "Convert count to for_each",
			refused: "the value of var.names is not known statically",
		},
		{
			name:   "duplicates",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0, env: listValue("a", "a"),
			title:   "Convert count to for_each",
			refused: `var.names holds "a" twice, and toset() would drop an instance`,
		},
		{
			name:   "numbers",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0,
			env: func(env *Env) {
				env.StaticValue = func(dir string, expr hcl.Expression) StaticValue {
					return StaticValue{Value: cty.ListVal([]cty.Value{cty.NumberIntVal(1)}), Known: true}
				}
			},
			title:   "Convert count to for_each",
			refused: "var.names holds a value that is not a string, and for_each keys must be strings",
		},
		{
			name:   "the order of a splat",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0, env: listValue("b", "a"),
			title:   "Convert count to for_each",
			refused: "terraform_data.t[*] at main.tf:13 lists the instances in the order of var.names, but values(terraform_data.t) would list them by key",
		},
		{
			name:   "count.index elsewhere",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = length(var.names)\n  input = \"n-${count.index}\"\n}\n"},
			needle: "count", cursor: 0, env: listValue("a"),
			title:   "Convert count to for_each",
			refused: "count.index at main.tf:3 is used other than as var.names[count.index], and each.value cannot replace it",
		},
		{
			name:   "an index past the instances",
			files:  files{"main.tf": countTF},
			needle: "count", cursor: 0, env: listValue(),
			title:   "Convert count to for_each",
			refused: "terraform_data.t[0] at main.tf:9 is not one of the 0 instances",
		},
		{
			name:   "an index by an expression",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = length(var.names)\n}\n\noutput \"o\" {\n  value = terraform_data.t[var.i].id\n}\n"},
			needle: "count", cursor: 0, env: listValue("a"),
			title:   "Convert count to for_each",
			refused: "terraform_data.t at main.tf:6 is a list of instances, which for_each turns into a map",
		},
		{
			name:   "a moved block",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = length(var.names)\n}\n\nmoved {\n  from = terraform_data.old\n  to   = terraform_data.t[0]\n}\n"},
			needle: "count", cursor: 0, env: listValue("a"),
			title:   "Convert count to for_each",
			refused: "a moved block refers to terraform_data.t at main.tf:7",
		},
		{
			name:   "a count of its own",
			files:  files{"main.tf": "resource \"terraform_data\" \"t\" {\n  count = 3\n}\n"},
			needle: "count", cursor: 0,
			title:   "Convert count to for_each",
			refused: "count is neither length(<list>) nor <condition> ? 1 : 0",
		},
	})
}

const deleteTF = `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept  = var.used
  spare = "s"
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}

output "kept" {
  value = local.kept
}
`

func TestSafeDelete(t *testing.T) {
	runRefactorCases(t, []refactorCase{
		{
			name:   "an unused variable",
			files:  files{"main.tf": deleteTF},
			needle: `"unused"`, cursor: 1,
			title: `Safe delete variable "unused"`,
			edits: []string{
				`main.tf 5:1-9:1 ""`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept  = var.used
  spare = "s"
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}

output "kept" {
  value = local.kept
}
`,
			},
		},
		{
			name:   "a variable used only by its validation",
			files:  files{"main.tf": deleteTF},
			needle: `variable "checked"`, cursor: 0,
			title: `Safe delete variable "checked"`,
			edits: []string{
				`main.tf 9:1-18:1 ""`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

locals {
  kept  = var.used
  spare = "s"
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}

output "kept" {
  value = local.kept
}
`,
			},
		},
		{
			name:   "a used variable lists its uses",
			files:  files{"main.tf": deleteTF, "tests/a.tftest.hcl": "run \"r\" {\n  assert {\n    condition     = var.used != \"\"\n    error_message = \"a\"\n  }\n}\n"},
			needle: `"used"`, cursor: 1,
			title:   `Safe delete variable "used"`,
			refused: "var.used is used twice: main.tf:19, tests/a.tftest.hcl:3",
		},
		{
			name:   "an unused local",
			files:  files{"main.tf": deleteTF},
			needle: "spare", cursor: 0,
			title: `Safe delete local "spare"`,
			edits: []string{
				`main.tf 19:1-21:1 "  kept = var.used\n"`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept = var.used
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}

output "kept" {
  value = local.kept
}
`,
			},
		},
		{
			name:   "a used local",
			files:  files{"main.tf": deleteTF},
			needle: "kept ", cursor: 0,
			title:   `Safe delete local "kept"`,
			refused: "local.kept is used once: main.tf:34",
		},
		{
			name:   "an unused data source",
			files:  files{"main.tf": deleteTF},
			needle: `"f"`, cursor: 1,
			title: "Safe delete data source data.local_file.f",
			edits: []string{
				`main.tf 23:1-27:1 ""`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept  = var.used
  spare = "s"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}

output "kept" {
  value = local.kept
}
`,
			},
		},
		{
			name:   "an unused data source of a check",
			files:  files{"main.tf": deleteTF},
			needle: `"g"`, cursor: 1,
			title: "Safe delete data source data.local_file.g",
			edits: []string{
				`main.tf 28:1-31:1 ""`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept  = var.used
  spare = "s"
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
}

output "kept" {
  value = local.kept
}
`,
			},
		},
		{
			name:   "an output nothing reads",
			files:  files{"main.tf": deleteTF},
			needle: `output "kept"`, cursor: 0,
			title: `Safe delete output "kept"`,
			edits: []string{
				`main.tf 32:1-36:1 ""`,
			},
			want: files{
				"main.tf": `variable "used" {
  type = string
}

variable "unused" {
  type = string
}

variable "checked" {
  type = string

  validation {
    condition     = length(var.checked) > 0
    error_message = "Must not be empty."
  }
}

locals {
  kept  = var.used
  spare = "s"
}

data "local_file" "f" {
  filename = "a.txt"
}

check "c" {
  data "local_file" "g" {
    filename = "b.txt"
  }
}
`,
			},
		},
		{
			name:   "an output a caller reads",
			files:  files{"m/main.tf": "output \"id\" {\n  value = 1\n}\n", "main.tf": "module \"m\" {\n  source = \"./m\"\n}\n\noutput \"x\" {\n  value = module.m.id\n}\n"},
			doc:    "m/main.tf",
			needle: `"id"`, cursor: 1,
			env: func(env *Env) {
				env.ModuleCallers = func(dir string) ([]ModuleCaller, bool) {
					return []ModuleCaller{{Dir: root, Name: "m"}}, true
				}
			},
			title:   `Safe delete output "id"`,
			refused: "output.id is used once: ../main.tf:6",
		},
		{
			name:   "an output a caller not indexed yet reads: refused when resolved",
			files:  files{"m/main.tf": "output \"id\" {\n  value = 1\n}\n", "main.tf": "module \"m\" {\n  source = \"./m\"\n}\n\noutput \"x\" {\n  value = module.m[*].id\n}\n"},
			doc:    "m/main.tf",
			needle: `"id"`, cursor: 1,
			env: func(env *Env) {
				env.IndexedCallers = func(dir string) []ModuleCaller { return nil }
				env.ModuleCallers = func(dir string) ([]ModuleCaller, bool) {
					return []ModuleCaller{{Dir: root, Name: "m"}}, true
				}
			},
			title:   `Safe delete output "id"`,
			refused: "output.id is used once: ../main.tf:6",
		},
		{
			name:   "an output a test reads",
			files:  files{"main.tf": deleteTF, "a.tftest.hcl": "run \"r\" {\n  assert {\n    condition     = output.kept != \"\"\n    error_message = \"a\"\n  }\n}\n"},
			needle: `output "kept"`, cursor: 0,
			title:   `Safe delete output "kept"`,
			refused: "output.kept is used once: a.tftest.hcl:3",
		},
		{
			name:   "not a resource",
			files:  files{"main.tf": "resource \"terraform_data\" \"r\" {}\n"},
			needle: `"r"`, cursor: 1,
			title:  `Safe delete resource "r"`,
			absent: true,
		},
	})
}
