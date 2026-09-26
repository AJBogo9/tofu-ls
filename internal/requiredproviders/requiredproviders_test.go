// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package requiredproviders

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func parseFiles(t *testing.T, srcs map[string]string) map[string]*hcl.File {
	t.Helper()
	files := make(map[string]*hcl.File, len(srcs))
	for name, src := range srcs {
		f, _ := hclsyntax.ParseConfig([]byte(src), name, hcl.InitialPos)
		files[name] = f
	}
	return files
}

func applyEdit(src string, e Edit) string {
	return src[:e.Range.Start.Byte] + e.NewText + src[e.Range.End.Byte:]
}

func TestAddEntryEdit(t *testing.T) {
	testCases := []struct {
		name         string
		files        map[string]string
		wantFile     string
		wantText     string
		wantNoTarget bool
	}{
		{
			"into the required_providers block of another file",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
				"versions.tf": `terraform {
  required_version = ">= 1.6"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}
`,
			},
			"versions.tf",
			`terraform {
  required_version = ">= 1.6"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"after a one-line entry",
			map[string]string{
				"main.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`,
			},
			"main.tf",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random" }
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"empty required_providers on one line",
			map[string]string{
				"main.tf": `terraform {
  required_providers {}
}
`,
			},
			"main.tf",
			`terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"terraform block without required_providers",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
				"terraform.tf": `terraform {
  required_version = ">= 1.6"
}
`,
			},
			"terraform.tf",
			`terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"no terraform block, a versions.tf",
			map[string]string{
				"main.tf":     "resource \"aws_instance\" \"a\" {}\n",
				"versions.tf": "# provider requirements\n",
			},
			"versions.tf",
			`# provider requirements

terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"no terraform block, top of the current file below the header",
			map[string]string{
				"main.tf": `# Copyright header

# the instance
resource "aws_instance" "a" {}
`,
			},
			"main.tf",
			`# Copyright header

terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

# the instance
resource "aws_instance" "a" {}
`,
			false,
		},
		{
			"no terraform block, current file starts with the block",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
			},
			"main.tf",
			`terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

resource "aws_instance" "a" {}
`,
			false,
		},
		{
			"single-line required_providers block with an entry",
			map[string]string{
				"main.tf": "terraform {\n  required_providers { random = { source = \"hashicorp/random\" } }\n}\n",
			},
			"main.tf",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random" }
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false,
		},
		{
			"only a JSON file",
			map[string]string{
				"main.tf.json": `{"resource": {}}`,
			},
			"",
			"",
			true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files := parseFiles(t, tc.files)
			current := "main.tf"
			if _, ok := tc.files[current]; !ok {
				current = "main.tf.json"
			}
			edit, ok := AddEntryEdit(files, current, "aws", "hashicorp/aws", EntryOptions{})
			if tc.wantNoTarget {
				if ok {
					t.Fatalf("expected no edit, got %#v", edit)
				}
				return
			}
			if !ok {
				t.Fatal("expected an edit")
			}
			if edit.Filename != tc.wantFile {
				t.Fatalf("edit in %q, want %q", edit.Filename, tc.wantFile)
			}
			got := applyEdit(tc.files[edit.Filename], edit)
			if diff := cmp.Diff(tc.wantText, got); diff != "" {
				t.Fatalf("unexpected text: %s", diff)
			}
		})
	}
}

func TestAddEntryEdit_options(t *testing.T) {
	testCases := []struct {
		name     string
		files    map[string]string
		opts     EntryOptions
		wantFile string
		// wantText is the file after the edit, or the new file
		wantText     string
		wantNewBlock bool
		wantCreate   bool
	}{
		{
			"with a version constraint",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
				"versions.tf": `terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
    }
  }
}
`,
			},
			EntryOptions{Version: "~> 6.0"},
			"versions.tf",
			`terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}
`,
			false, false,
		},
		{
			"the terraform block of versions.tf before an earlier one",
			map[string]string{
				"main.tf":     "resource \"aws_instance\" \"a\" {}\n",
				"backend.tf":  "terraform {\n  backend \"local\" {}\n}\n",
				"versions.tf": "terraform {\n  required_version = \">= 1.6\"\n}\n",
			},
			EntryOptions{},
			"versions.tf",
			`terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false, false,
		},
		{
			"first in an empty terraform block: no blank line",
			map[string]string{
				"main.tf":     "resource \"aws_instance\" \"a\" {}\n",
				"versions.tf": "terraform {\n}\n",
			},
			EntryOptions{},
			"versions.tf",
			`terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			false, false,
		},
		{
			"a new versions.tf, for a client which can create it",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
			},
			EntryOptions{CreateVersionsFile: true},
			"versions.tf",
			`terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			true, true,
		},
		{
			"versions.tf which ends with a blank line",
			map[string]string{
				"main.tf":     "resource \"aws_instance\" \"a\" {}\n",
				"versions.tf": "# versions\n\n",
			},
			EntryOptions{CreateVersionsFile: true},
			"versions.tf",
			`# versions

terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
			true, false,
		},
		{
			"the line endings of the file",
			map[string]string{
				"main.tf": "terraform {\r\n  required_providers {\r\n  }\r\n}\r\n\r\nresource \"aws_instance\" \"a\" {}\r\n",
			},
			EntryOptions{},
			"main.tf",
			"terraform {\r\n  required_providers {\r\n    aws = {\r\n      source = \"hashicorp/aws\"\r\n    }\r\n  }\r\n}\r\n\r\nresource \"aws_instance\" \"a\" {}\r\n",
			false, false,
		},
		{
			"a new terraform block at the top of the current file",
			map[string]string{
				"main.tf": "resource \"aws_instance\" \"a\" {}\n",
			},
			EntryOptions{Version: "~> 6.0"},
			"main.tf",
			`terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

resource "aws_instance" "a" {}
`,
			true, false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			edit, ok := AddEntryEdit(parseFiles(t, tc.files), "main.tf", "aws", "hashicorp/aws", tc.opts)
			if !ok {
				t.Fatal("expected an edit")
			}
			if edit.Filename != tc.wantFile || edit.NewBlock != tc.wantNewBlock || edit.Create != tc.wantCreate {
				t.Fatalf("edit of %q (new block %v, create %v), want %q (%v, %v)",
					edit.Filename, edit.NewBlock, edit.Create, tc.wantFile, tc.wantNewBlock, tc.wantCreate)
			}
			if diff := cmp.Diff(tc.wantText, applyEdit(tc.files[edit.Filename], edit)); diff != "" {
				t.Fatalf("unexpected text: %s", diff)
			}
		})
	}
}

func TestEntries(t *testing.T) {
	files := parseFiles(t, map[string]string{
		"a.tf": `terraform {
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.0" }
    random = "~> 3.0"
    github = {
      "source" = "integrations/github"
    }
  }
}
`,
		"b.tf": `terraform {
  required_providers {
    aws = { source = "acme/aws" }
    tls = {}
  }
}
`,
	})
	got := make(map[string]string)
	for name, e := range Entries(files) {
		got[name] = e.Filename + " " + e.Source
	}
	want := map[string]string{
		"aws":    "a.tf hashicorp/aws",
		"random": "a.tf ",
		"github": "a.tf integrations/github",
		"tls":    "b.tf ",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected entries: %s", diff)
	}
}

func TestEntryAtPos(t *testing.T) {
	src := `terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ""
    }
  }
}
`
	files := parseFiles(t, map[string]string{"main.tf": src})
	testCases := []struct {
		pos      hcl.Pos
		wantName string
		wantOK   bool
	}{
		{hcl.Pos{Line: 5, Column: 18, Byte: 97}, "aws", true},
		{hcl.Pos{Line: 4, Column: 20, Byte: 64}, "aws", true},
		// on the key
		{hcl.Pos{Line: 3, Column: 6, Byte: 38}, "", false},
		{hcl.Pos{Line: 1, Column: 1, Byte: 0}, "", false},
	}
	for _, tc := range testCases {
		e, ok := EntryAtPos(files["main.tf"], "main.tf", tc.pos)
		if ok != tc.wantOK || e.Name != tc.wantName {
			t.Fatalf("at %v: got %q %v, want %q %v", tc.pos, e.Name, ok, tc.wantName, tc.wantOK)
		}
		if ok && e.Source != "hashicorp/aws" {
			t.Fatalf("unexpected source %q", e.Source)
		}
	}
}

func TestNewEntryAtPos(t *testing.T) {
	testCases := []struct {
		name string
		src  string
		want bool
	}{
		{
			"empty line in the body",
			"terraform {\n  required_providers {\n    aws = { source = \"hashicorp/aws\" }\n    |\n  }\n}\n",
			true,
		},
		{
			"half-typed name, which breaks the block",
			"terraform {\n  required_providers {\n    te|\n  }\n}\n",
			true,
		},
		{
			"empty body on one line, not a line of its own",
			"terraform {\n  required_providers {|}\n}\n",
			false,
		},
		{
			"on an entry key",
			"terraform {\n  required_providers {\n    a|ws = { source = \"hashicorp/aws\" }\n  }\n}\n",
			false,
		},
		{
			"inside an entry value",
			"terraform {\n  required_providers {\n    aws = {\n      |\n    }\n  }\n}\n",
			false,
		},
		{
			"in the terraform block",
			"terraform {\n  |\n  required_providers {\n  }\n}\n",
			false,
		},
		{
			"required_providers outside of terraform",
			"locals {\n  required_providers {\n    |\n  }\n}\n",
			false,
		},
		{
			"after the block",
			"terraform {\n  required_providers {\n  }\n}\n|",
			false,
		},
		{
			"after an interpolation in the terraform block",
			"terraform {\n  x = \"${a}\"\n  required_providers {\n    |\n  }\n}\n",
			true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			offset := strings.Index(tc.src, "|")
			src := []byte(strings.Replace(tc.src, "|", "", 1))
			pos := hcl.Pos{
				Line:   strings.Count(tc.src[:offset], "\n") + 1,
				Column: offset - strings.LastIndex(tc.src[:offset], "\n"),
				Byte:   offset,
			}
			if got := NewEntryAtPos(src, "main.tf", pos); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUsedLocalNames(t *testing.T) {
	files := parseFiles(t, map[string]string{
		"main.tf": `resource "aws_instance" "a" {}
resource "aws_instance" "b" {
  provider = google-beta.west
}
data "http" "x" {}
ephemeral "random_password" "p" {}
resource "terraform_data" "t" {}
provider "tls" {}
check "c" {
  data "dns_a_record_set" "d" {}
}
`,
	})
	want := []string{"aws", "dns", "google-beta", "http", "random", "tls"}
	if diff := cmp.Diff(want, UsedLocalNames(files)); diff != "" {
		t.Fatalf("unexpected names: %s", diff)
	}
}
