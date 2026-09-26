// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"testing"
)

const testLockFile = `# This file is maintained automatically by "tofu init".

provider "registry.opentofu.org/hashicorp/random" {
  version     = "3.6.2"
  constraints = ">= 3.0.0"
  hashes = [
    "h1:abc=",
  ]
}
`

func TestAddRequiredProvider(t *testing.T) {
	testCases := []struct {
		name    string
		files   files
		needle  string
		create  bool
		title   string
		edits   []string
		want    files
		creates string
	}{
		{
			name: "next to the declared providers, with the locked version",
			files: files{
				"main.tf": "resource \"random_pet\" \"p\" {}\n",
				"versions.tf": `terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}
`,
				".terraform.lock.hcl": testLockFile,
			},
			needle: `"random_pet"`,
			title:  `Add "random" to required_providers`,
			edits:  []string{`versions.tf 6:1-6:1 "    random = {\n      source  = \"hashicorp/random\"\n      version = \"~> 3.6\"\n    }\n"`},
			want: files{
				"versions.tf": `terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
`,
			},
		},
		{
			name: "a terraform block without required_providers",
			files: files{
				"main.tf": `terraform {
  required_version = ">= 1.6"
}

data "local_file" "f" {
  filename = "a"
}
`,
			},
			needle: `"local_file"`,
			title:  `Add "local" to required_providers`,
			edits:  []string{`main.tf 3:1-3:1 "\n  required_providers {\n    local = {\n      source = \"hashicorp/local\"\n    }\n  }\n"`},
			want: files{
				"main.tf": `terraform {
  required_version = ">= 1.6"

  required_providers {
    local = {
      source = "hashicorp/local"
    }
  }
}

data "local_file" "f" {
  filename = "a"
}
`,
			},
		},
		{
			name:    "a new versions.tf",
			files:   files{"main.tf": "resource \"null_resource\" \"n\" {}\n"},
			needle:  `"null_resource"`,
			create:  true,
			title:   `Add "null" to required_providers in a new versions.tf`,
			edits:   []string{`versions.tf 1:1-1:1 "terraform {\n  required_providers {\n    null = {\n      source = \"hashicorp/null\"\n    }\n  }\n}\n"`},
			creates: "versions.tf",
			want: files{
				"versions.tf": "terraform {\n  required_providers {\n    null = {\n      source = \"hashicorp/null\"\n    }\n  }\n}\n",
			},
		},
		{
			name: "the provider meta-argument names the provider",
			files: files{
				"main.tf":     "resource \"random_pet\" \"p\" {\n  provider = rnd.eu\n}\n",
				"versions.tf": "# versions\n",
			},
			needle: `"random_pet"`,
			title:  `Add "rnd" to required_providers in versions.tf`,
			edits:  []string{`versions.tf 2:1-2:1 "\nterraform {\n  required_providers {\n    rnd = {\n      source = \"hashicorp/rnd\"\n    }\n  }\n}\n"`},
			want: files{
				"versions.tf": "# versions\n\nterraform {\n  required_providers {\n    rnd = {\n      source = \"hashicorp/rnd\"\n    }\n  }\n}\n",
			},
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			env := tc.files.newEnv()
			env.CreateFiles = tc.create
			pos := tc.files.rangeOf(t, "main.tf", tc.needle, 1).Start
			pos = posAt([]byte(tc.files["main.tf"]), pos.Byte+3)
			actions := Intentions(env, tc.files.doc("main.tf"), pos)
			a := expectAction(t, tc.files, actions, tc.title, tc.edits, tc.want)
			if a.Kind != KindRefactorRewrite {
				t.Errorf("expected a rewrite, got %q", a.Kind)
			}
			if tc.creates != "" && (len(a.Create) != 1 || a.Create[0] != filepath.Join(root, tc.creates)) {
				t.Errorf("expected to create %s, got %v", tc.creates, a.Create)
			}
		})
	}
}

func TestAddRequiredProvider_notOffered(t *testing.T) {
	testCases := []struct {
		name   string
		files  files
		needle string
	}{
		{"already declared", files{
			"main.tf": "terraform {\n  required_providers {\n    random = {\n      source = \"hashicorp/random\"\n    }\n  }\n}\n\nresource \"random_pet\" \"p\" {}\n",
		}, `"random_pet"`},
		{"declared in another file", files{
			"main.tf":     "resource \"random_pet\" \"p\" {}\n",
			"versions.tf": "terraform {\n  required_providers {\n    random = {\n      source = \"hashicorp/random\"\n    }\n  }\n}\n",
		}, `"random_pet"`},
		{"the built-in provider", files{"main.tf": "resource \"terraform_data\" \"d\" {}\n"}, `"terraform_data"`},
		{"on the name label", files{"main.tf": "resource \"random_pet\" \"pet\" {}\n"}, `"pet"`},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			pos := tc.files.rangeOf(t, "main.tf", tc.needle, 1).Start
			pos = posAt([]byte(tc.files["main.tf"]), pos.Byte+2)
			if actions := Intentions(tc.files.newEnv(), tc.files.doc("main.tf"), pos); len(actions) != 0 {
				t.Fatalf("expected no action, got %#v", actions)
			}
		})
	}
}
