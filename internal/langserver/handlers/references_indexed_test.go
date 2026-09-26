// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
)

// span is the location of text on the 0-based line of a file (ASCII
// sources only).
func span(t *testing.T, tmpDir document.DirHandle, files map[string]string, file string, line int, text string) string {
	t.Helper()
	l := strings.Split(files[file], "\n")[line]
	i := strings.Index(l, text)
	if i < 0 {
		t.Fatalf("%q not on %s:%d: %q", text, file, line, l)
	}
	return fmt.Sprintf(`{"uri": "%s/%s", "range": {"start": {"line": %d, "character": %d}, "end": {"line": %d, "character": %d}}}`,
		tmpDir.URI, file, line, i, line, i+len(text))
}

const indexedMainTf = `variable "names" {
  type    = list(string)
  default = ["a", "b"]
}

resource "terraform_data" "gw" {
  count = 2
  input = "gw-${count.index}"
}

resource "terraform_data" "user" {
  input = terraform_data.gw[0].id
}

module "kids" {
  source   = "./kid"
  for_each = toset(var.names)
  label    = each.key
}

module "child" {
  source = "./kid"
  count  = 1
  label  = "c"
}

locals {
  a     = module.kids["a"].name
  dyn   = module.kids[var.names[1]].name
  splat = module.child[*].name
  first = module.child[0].name
  all   = terraform_data.gw[*].id
  one   = terraform_data.gw[1].output
  last  = terraform_data.gw[length(var.names) - 1].id
}
`

const indexedKidTf = `variable "label" {
  type = string
}

output "name" {
  value = "kid-${var.label}"
}
`

func TestReferences_indexedAndSplatUses(t *testing.T) {
	files := map[string]string{"main.tf": indexedMainTf, "kid/main.tf": indexedKidTf}
	ls, tmpDir, stop := startNavigationServerOpening(t, files, "{}", "kid/main.tf")
	defer stop()

	// counted resources (terraform_data.gw[0].id) need the provider's
	// schema, which these servers do not load: see
	// TestTargets_Match_resourceInstanceKey in hcl-lang
	testCases := []struct {
		name     string
		cursor   at
		expected []string
	}{
		{
			// static and dynamic keys, a splat and a count index
			"output of a module called with for_each and count",
			at{"kid/main.tf", 4, "name", 1, 1},
			[]string{
				span(t, tmpDir, files, "main.tf", 27, `module.kids["a"].name`),
				span(t, tmpDir, files, "main.tf", 28, `module.kids[var.names[1]].name`),
				span(t, tmpDir, files, "main.tf", 29, `module.child[*].name`),
				span(t, tmpDir, files, "main.tf", 30, `module.child[0].name`),
			},
		},
		{
			"list variable read by index",
			at{"main.tf", 0, "names", 1, 1},
			[]string{
				span(t, tmpDir, files, "main.tf", 16, `var.names`),
				span(t, tmpDir, files, "main.tf", 28, `var.names[1]`),
				span(t, tmpDir, files, "main.tf", 33, `var.names`),
			},
		},
	}
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ls.CallAndExpectResponse(t, &langserver.CallRequest{
				Method: "textDocument/references",
				ReqParams: fmt.Sprintf(`{
					"textDocument": {"uri": "%s/%s"},
					"position": %s,
					"context": {"includeDeclaration": false}
				}`, tmpDir.URI, tc.cursor.file, tc.cursor.position(t, files))}, fmt.Sprintf(`{
					"jsonrpc": "2.0",
					"id": %d,
					"result": [%s]
				}`, i+4, strings.Join(tc.expected, ", ")))
		})
	}
}

func TestDefinition_indexedUses(t *testing.T) {
	files := map[string]string{"main.tf": indexedMainTf, "kid/main.tf": indexedKidTf}
	ls, tmpDir, stop := startNavigationServer(t, files)
	defer stop()

	block := func(file string, start, end int) string {
		return fmt.Sprintf(`{"uri": "%s/%s", "range": {"start": {"line": %d, "character": 0}, "end": {"line": %d, "character": 1}}}`,
			tmpDir.URI, file, start, end)
	}
	testCases := []struct {
		name     string
		cursor   at
		expected []string
	}{
		{"variable read by index", at{"main.tf", 28, "names", 1, 1}, []string{block("main.tf", 0, 3)}},
		// on the call name: the call only, as references resolve it
		{"module call in a splat", at{"main.tf", 29, "child", 1, 1}, []string{block("main.tf", 20, 24)}},
	}
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ls.CallAndExpectResponse(t, &langserver.CallRequest{
				Method: "textDocument/definition",
				ReqParams: fmt.Sprintf(`{
					"textDocument": {"uri": "%s/%s"},
					"position": %s
				}`, tmpDir.URI, tc.cursor.file, tc.cursor.position(t, files))}, fmt.Sprintf(`{
					"jsonrpc": "2.0",
					"id": %d,
					"result": [%s]
				}`, i+3, strings.Join(tc.expected, ", ")))
		})
	}
}

func TestDefinitionAndReferences_variableInBackend(t *testing.T) {
	files := map[string]string{
		"main.tf": `variable "bucket" {
  type = string
}

terraform {
  backend "local" {
    path = var.bucket
  }
}
`,
	}
	ls, tmpDir, stop := startNavigationServer(t, files)
	defer stop()

	// OpenTofu evaluates the backend early, from variables and locals
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/definition",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 6, "character": 17}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [{"uri": "%s/main.tf", "range": {"start": {"line": 0, "character": 0}, "end": {"line": 2, "character": 1}}}]
		}`, tmpDir.URI))
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/references",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12},
			"context": {"includeDeclaration": false}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": [%s]
		}`, span(t, tmpDir, files, "main.tf", 6, "var.bucket")))
}

func TestSelectionRangeAndReferences_emojiLine(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf + "\nlocals {\n  rocket = \"\U0001F680 ${var.stage}\"\n}\n",
	})
	defer stop()

	// LSP counts the emoji as two UTF-16 code units, HCL as one column
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/selectionRange",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"positions": [{"line": 17, "character": 23}]
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"range": {"start": {"line": 17, "character": 21}, "end": {"line": 17, "character": 26}},
				"parent": {"range": {"start": {"line": 17, "character": 17}, "end": {"line": 17, "character": 26}},
				"parent": {"range": {"start": {"line": 17, "character": 11}, "end": {"line": 17, "character": 28}},
				"parent": {"range": {"start": {"line": 17, "character": 2}, "end": {"line": 17, "character": 28}},
				"parent": {"range": {"start": {"line": 16, "character": 8}, "end": {"line": 18, "character": 0}},
				"parent": {"range": {"start": {"line": 16, "character": 7}, "end": {"line": 18, "character": 1}},
				"parent": {"range": {"start": {"line": 16, "character": 0}, "end": {"line": 18, "character": 1}},
				"parent": {"range": {"start": {"line": 0, "character": 0}, "end": {"line": 19, "character": 0}}}}}}}}}}
			]
		}`)

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/references",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 17, "character": 23},
			"context": {"includeDeclaration": false}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": [
				{"uri": "%s/main.tf", "range": {"start": {"line": 5, "character": 16}, "end": {"line": 5, "character": 25}}},
				{"uri": "%s/main.tf", "range": {"start": {"line": 17, "character": 17}, "end": {"line": 17, "character": 26}}}
			]
		}`, tmpDir.URI, tmpDir.URI))
}

func TestDefinitionAndReferences_localNamedLikeProviderAlias(t *testing.T) {
	files := map[string]string{
		"main.tf": `provider "local" {
  alias = "secondary"
}

locals {
  secondary = "not a provider"
}

resource "terraform_data" "a" {
  provider = local.secondary
  input    = local.secondary
}
`,
	}
	ls, tmpDir, stop := startNavigationServer(t, files)
	defer stop()

	loc := func(startLine, startChar, endLine, endChar int) string {
		return fmt.Sprintf(`{"uri": "%s/main.tf", "range": {"start": {"line": %d, "character": %d}, "end": {"line": %d, "character": %d}}}`,
			tmpDir.URI, startLine, startChar, endLine, endChar)
	}
	testCases := []struct {
		name     string
		method   string
		cursor   at
		expected string
	}{
		// the provider meta-argument names the provider
		{"definition from the provider meta-argument", "textDocument/definition", at{"main.tf", 9, "secondary", 1, 1}, loc(0, 0, 2, 1)},
		// elsewhere local.secondary is the local value
		{"definition from a value", "textDocument/definition", at{"main.tf", 10, "secondary", 1, 1}, loc(5, 2, 5, 30)},
		{"references of the provider", "textDocument/references", at{"main.tf", 0, "local", 1, 1}, span(t, tmpDir, files, "main.tf", 9, "local.secondary")},
		{"references of the local value", "textDocument/references", at{"main.tf", 5, "secondary", 1, 1}, span(t, tmpDir, files, "main.tf", 10, "local.secondary")},
	}
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ls.CallAndExpectResponse(t, &langserver.CallRequest{
				Method: tc.method,
				ReqParams: fmt.Sprintf(`{
					"textDocument": {"uri": "%s/main.tf"},
					"position": %s,
					"context": {"includeDeclaration": false}
				}`, tmpDir.URI, tc.cursor.position(t, files))}, fmt.Sprintf(`{
					"jsonrpc": "2.0",
					"id": %d,
					"result": [%s]
				}`, i+3, tc.expected))
		})
	}
}
