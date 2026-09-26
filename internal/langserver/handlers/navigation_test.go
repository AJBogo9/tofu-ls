// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/go-version"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

const navigationMainTf = `variable "stage" {
  default = "dev"
}

locals {
  name = "app-${var.stage}"
}

resource "terraform_data" "web" {
  input = local.name
}

output "id" {
  value = terraform_data.web.id
}
`

// startNavigationServer writes files into a temporary module,
// opens main.tf and waits until the module is indexed.
type navigationServer interface {
	CallAndExpectResponse(t *testing.T, cr *langserver.CallRequest, expectRaw string)
	CallAndExpectError(t *testing.T, cr *langserver.CallRequest, expectErr error)
}

func startNavigationServer(t *testing.T, files map[string]string) (navigationServer, document.DirHandle, func()) {
	return startNavigationServerWithOptions(t, files, "{}")
}

func startNavigationServerWithOptions(t *testing.T, files map[string]string, initOptions string) (navigationServer, document.DirHandle, func()) {
	return startNavigationServerOpening(t, files, initOptions)
}

// startNavigationServerOpening also opens the files named in open, besides
// main.tf.
func startNavigationServerOpening(t *testing.T, files map[string]string, initOptions string, open ...string) (navigationServer, document.DirHandle, func()) {
	tmpDir := TempDir(t)
	for name, content := range files {
		path := filepath.Join(tmpDir.Path(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	wc := walker.NewWalkerCollector()

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				tmpDir.Path(): {
					{
						Method:        "Version",
						Repeatability: 1,
						Arguments: []interface{}{
							mock.AnythingOfType(""),
						},
						ReturnArguments: []interface{}{
							version.Must(version.NewVersion("1.9.0")),
							nil,
							nil,
						},
					},
					{
						Method:        "GetExecPath",
						Repeatability: 1,
						ReturnArguments: []interface{}{
							"",
						},
					},
				},
			},
		},
		StateStore:      ss,
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {},
	    "initializationOptions": %s,
	    "rootUri": %q,
	    "processId": 12345
	}`, initOptions, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "opentofu",
			"text": %q,
			"uri": "%s/main.tf"
		}
	}`, files["main.tf"], tmpDir.URI)})
	for _, name := range open {
		ls.Call(t, &langserver.CallRequest{
			Method: "textDocument/didOpen",
			ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "opentofu",
			"text": %q,
			"uri": "%s/%s"
		}
	}`, files[name], tmpDir.URI, name)})
	}
	waitForAllJobs(t, ss)

	return ls, tmpDir, stop
}

func TestRename_variable(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf":          navigationMainTf,
		"terraform.tfvars": "stage = \"prod\"\n",
	})
	defer stop()

	// on the "stage" label of the variable block
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/prepareRename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}},
				"placeholder": "stage"
			}
		}`)

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 5, "character": 20},
			"newName": "tier"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}}, "newText": "tier"},
						{"range": {"start": {"line": 5, "character": 20}, "end": {"line": 5, "character": 25}}, "newText": "tier"}
					],
					"%s/terraform.tfvars": [
						{"range": {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 5}}, "newText": "tier"}
					]
				}
			}
		}`, tmpDir.URI, tmpDir.URI))
}

func TestRename_resourceAddsMovedBlock(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	// on "web" in terraform_data.web.id
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 13, "character": 27},
			"newName": "api"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 8, "character": 27}, "end": {"line": 8, "character": 30}}, "newText": "api"},
						{"range": {"start": {"line": 13, "character": 25}, "end": {"line": 13, "character": 28}}, "newText": "api"},
						{"range": {"start": {"line": 15, "character": 0}, "end": {"line": 15, "character": 0}},
						 "newText": "\nmoved {\n  from = terraform_data.web\n  to   = terraform_data.api\n}\n"}
					]
				}
			}
		}`, tmpDir.URI))
}

func TestRename_refusesCollision(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf + "\nvariable \"tier\" {}\n",
	})
	defer stop()

	ls.CallAndExpectError(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12},
			"newName": "tier"
		}`, tmpDir.URI)}, &jrpc2.Error{Code: jrpc2.Code(-32098), Message: "var.tier already exists"})
}

func TestDocumentHighlight_onReference(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	// on local.name inside the resource: the local's declaration is
	// a write, its reference a read
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/documentHighlight",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 9, "character": 17}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"range": {"start": {"line": 5, "character": 2}, "end": {"line": 5, "character": 6}}, "kind": 3},
				{"range": {"start": {"line": 9, "character": 10}, "end": {"line": 9, "character": 20}}, "kind": 2}
			]
		}`)
}

func TestReferences_onReferenceInsideDeclaration(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	// on var.stage inside local.name: the references of var.stage,
	// not those of the enclosing local.name
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/references",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 5, "character": 20},
			"context": {"includeDeclaration": true}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"uri": "%s/main.tf", "range": {"start": {"line": 5, "character": 16}, "end": {"line": 5, "character": 25}}}
			]
		}`, tmpDir.URI))
}

func TestReferences_forEach(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": `module "web" {
  source   = "./web"
  for_each = toset(["a", "b"])
  stage    = each.key
}

output "web" {
  value = module.web
}
`,
		"web/main.tf": `variable "stage" {
}
`,
	})
	defer stop()

	eachKey := fmt.Sprintf(`{"uri": "%s/main.tf", "range": {"start": {"line": 3, "character": 13}, "end": {"line": 3, "character": 21}}}`, tmpDir.URI)
	module := fmt.Sprintf(`{"uri": "%s/main.tf", "range": {"start": {"line": 7, "character": 10}, "end": {"line": 7, "character": 20}}}`, tmpDir.URI)

	testCases := []struct {
		name      string
		character int
		expected  string
	}{
		// where the reference count lens of each.key and each.value
		// asks: their references only, as the lens counts them
		{"on the for_each name", 6, eachKey},
		// elsewhere in the argument (the cursor of the upstream
		// integration test, and a function in the expression): the
		// module call around it as well
		{"right after the for_each name", 10, eachKey + ", " + module},
		{"on toset", 15, eachKey + ", " + module},
	}
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ls.CallAndExpectResponse(t, &langserver.CallRequest{
				Method: "textDocument/references",
				ReqParams: fmt.Sprintf(`{
					"textDocument": {"uri": "%s/main.tf"},
					"position": {"line": 2, "character": %d},
					"context": {"includeDeclaration": false}
				}`, tmpDir.URI, tc.character)}, fmt.Sprintf(`{
					"jsonrpc": "2.0",
					"id": %d,
					"result": [%s]
				}`, i+3, tc.expected))
		})
	}
}

func TestFoldingRange(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/foldingRange",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"startLine": 0, "endLine": 1},
				{"startLine": 4, "endLine": 5},
				{"startLine": 8, "endLine": 9},
				{"startLine": 12, "endLine": 13}
			]
		}`)
}

func TestSelectionRange(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	// on "stage" in var.stage: the step, the reference, the template,
	// the attribute, the inside of the braces, the braces, the block,
	// the file
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/selectionRange",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"positions": [{"line": 5, "character": 21}]
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"range": {"start": {"line": 5, "character": 20}, "end": {"line": 5, "character": 25}},
				"parent": {"range": {"start": {"line": 5, "character": 16}, "end": {"line": 5, "character": 25}},
				"parent": {"range": {"start": {"line": 5, "character": 9}, "end": {"line": 5, "character": 27}},
				"parent": {"range": {"start": {"line": 5, "character": 2}, "end": {"line": 5, "character": 27}},
				"parent": {"range": {"start": {"line": 4, "character": 8}, "end": {"line": 6, "character": 0}},
				"parent": {"range": {"start": {"line": 4, "character": 7}, "end": {"line": 6, "character": 1}},
				"parent": {"range": {"start": {"line": 4, "character": 0}, "end": {"line": 6, "character": 1}},
				"parent": {"range": {"start": {"line": 0, "character": 0}, "end": {"line": 15, "character": 0}}}}}}}}}}
			]
		}`)
}

func TestPrepareRename_referenceInValidation(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": `variable "stage" {
  validation {
    condition     = contains(["dev", "prod"], var.stage)
    error_message = "Unknown stage."
  }
}
`,
	})
	defer stop()

	// the schema does not decode references in validation conditions,
	// so this one is read from the syntax
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/prepareRename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 2, "character": 52}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"range": {"start": {"line": 2, "character": 50}, "end": {"line": 2, "character": 55}},
				"placeholder": "stage"
			}
		}`)
}

func TestDocumentHighlight_declaration(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf,
	})
	defer stop()

	// on the declared name: the name and its reference in this file
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/documentHighlight",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}}, "kind": 3},
				{"range": {"start": {"line": 5, "character": 16}, "end": {"line": 5, "character": 25}}, "kind": 2}
			]
		}`)

	// on the block keyword: nothing, so the editor's word highlight applies
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/documentHighlight",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 2}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 4,
			"result": null
		}`)
}

func TestRename_resourceWithoutMovedBlock(t *testing.T) {
	ls, tmpDir, stop := startNavigationServerWithOptions(t, map[string]string{
		"main.tf": navigationMainTf + `
moved {
  from = terraform_data.old
  to   = terraform_data.web
}
`,
	}, `{"rename": {"addMovedBlock": false}}`)
	defer stop()

	// without a new moved block, the existing one follows the rename
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 8, "character": 28},
			"newName": "api"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 8, "character": 27}, "end": {"line": 8, "character": 30}}, "newText": "api"},
						{"range": {"start": {"line": 13, "character": 25}, "end": {"line": 13, "character": 28}}, "newText": "api"},
						{"range": {"start": {"line": 18, "character": 24}, "end": {"line": 18, "character": 27}}, "newText": "api"}
					]
				}
			}
		}`, tmpDir.URI))
}

func TestRename_resourceKeepsMovedChain(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf + `
moved {
  from = terraform_data.old
  to   = terraform_data.web
}
`,
	})
	defer stop()

	// the existing moved block keeps "to = terraform_data.web", so the
	// chain old -> web -> api stays valid: two moves into api would not be
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 8, "character": 28},
			"newName": "api"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 8, "character": 27}, "end": {"line": 8, "character": 30}}, "newText": "api"},
						{"range": {"start": {"line": 13, "character": 25}, "end": {"line": 13, "character": 28}}, "newText": "api"},
						{"range": {"start": {"line": 20, "character": 0}, "end": {"line": 20, "character": 0}},
						 "newText": "\nmoved {\n  from = terraform_data.web\n  to   = terraform_data.api\n}\n"}
					]
				}
			}
		}`, tmpDir.URI))
}

func TestRename_emojiLine(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": navigationMainTf + "\nlocals {\n  rocket = \"\U0001F680 ${var.stage}\"\n}\n",
	})
	defer stop()

	// LSP counts the emoji as two UTF-16 code units, HCL as one column
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/prepareRename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 17, "character": 23}
		}`, tmpDir.URI)}, `{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"range": {"start": {"line": 17, "character": 21}, "end": {"line": 17, "character": 26}},
				"placeholder": "stage"
			}
		}`)

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 17, "character": 23},
			"newName": "tier"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}}, "newText": "tier"},
						{"range": {"start": {"line": 5, "character": 20}, "end": {"line": 5, "character": 25}}, "newText": "tier"},
						{"range": {"start": {"line": 17, "character": 21}, "end": {"line": 17, "character": 26}}, "newText": "tier"}
					]
				}
			}
		}`, tmpDir.URI))
}

func TestRename_variableInSubdirVarFiles(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf":                navigationMainTf,
		"terraform.tfvars":       "stage = \"prod\"\n",
		"envs/blue.tfvars":       "# blue\nstage = \"blue\"\n",
		"envs/green.tfvars.json": `{"stage": "green"}`,
		// a module of its own: its var files set its own variables
		"other/main.tf":          "variable \"stage\" {}\n",
		"other/terraform.tfvars": "stage = \"other\"\n",
	})
	defer stop()

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 5, "character": 20},
			"newName": "tier"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"changes": {
					"%s/envs/blue.tfvars": [
						{"range": {"start": {"line": 1, "character": 0}, "end": {"line": 1, "character": 5}}, "newText": "tier"}
					],
					"%s/envs/green.tfvars.json": [
						{"range": {"start": {"line": 0, "character": 2}, "end": {"line": 0, "character": 7}}, "newText": "tier"}
					],
					"%s/main.tf": [
						{"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}}, "newText": "tier"},
						{"range": {"start": {"line": 5, "character": 20}, "end": {"line": 5, "character": 25}}, "newText": "tier"}
					],
					"%s/terraform.tfvars": [
						{"range": {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 5}}, "newText": "tier"}
					]
				}
			}
		}`, tmpDir.URI, tmpDir.URI, tmpDir.URI, tmpDir.URI))
}

func TestRename_outputWithIndexedModuleCalls(t *testing.T) {
	ls, tmpDir, stop := startNavigationServerOpening(t, map[string]string{
		"main.tf": `module "counted" {
  count  = 2
  source = "./app"
}
module "keyed" {
  for_each = toset(["a", "b"])
  source   = "./app"
}
locals {
  all   = module.counted[*].first_port
  first = module.counted[0].first_port
  a     = module.keyed["a"].first_port
  k     = module.keyed[local.key].first_port
  key   = "a"
}
`,
		"app/outputs.tf": "output \"first_port\" {\n  value = 80\n}\n",
	}, "{}", "app/outputs.tf")
	defer stop()

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/app/outputs.tf"},
			"position": {"line": 0, "character": 10},
			"newName": "port"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": {
				"changes": {
					"%s/app/outputs.tf": [
						{"range": {"start": {"line": 0, "character": 8}, "end": {"line": 0, "character": 18}}, "newText": "port"}
					],
					"%s/main.tf": [
						{"range": {"start": {"line": 9, "character": 28}, "end": {"line": 9, "character": 38}}, "newText": "port"},
						{"range": {"start": {"line": 10, "character": 28}, "end": {"line": 10, "character": 38}}, "newText": "port"},
						{"range": {"start": {"line": 11, "character": 28}, "end": {"line": 11, "character": 38}}, "newText": "port"},
						{"range": {"start": {"line": 12, "character": 34}, "end": {"line": 12, "character": 44}}, "newText": "port"}
					]
				}
			}
		}`, tmpDir.URI, tmpDir.URI))
}

func TestRename_outputRefusesForExpression(t *testing.T) {
	ls, tmpDir, stop := startNavigationServerOpening(t, map[string]string{
		"main.tf": `module "keyed" {
  for_each = toset(["a", "b"])
  source   = "./app"
}
locals {
  ports = [for k, m in module.keyed : m.first_port]
}
`,
		"app/outputs.tf": "output \"first_port\" {\n  value = 80\n}\n",
	}, "{}", "app/outputs.tf")
	defer stop()

	ls.CallAndExpectError(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/app/outputs.tf"},
			"position": {"line": 0, "character": 10},
			"newName": "port"
		}`, tmpDir.URI)}, &jrpc2.Error{Code: jrpc2.Code(-32098), Message: "module.keyed is iterated by a for expression (main.tf:6), whose uses of first_port cannot be renamed safely; rename them by hand"})
}

func TestRename_syntaxErrorNamesTheChildFile(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": `module "app" {
  source = "./modules/app"
  stage  = "dev"
}
`,
		"modules/app/variables.tf": "variable \"stage\" {\n}\n",
		"modules/app/main.tf":      "locals {\n  name = var.stage\n",
	})
	defer stop()

	// renaming from the root: the file is the child module's main.tf, not
	// the main.tf the user has open
	ls.CallAndExpectError(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 2, "character": 4},
			"newName": "env_name"
		}`, tmpDir.URI)}, &jrpc2.Error{Code: jrpc2.Code(-32098), Message: filepath.Join("modules", "app", "main.tf") + " has syntax errors; fix them before renaming"})
}

func TestRename_refusesMovedAndRemovedHistory(t *testing.T) {
	history := `
moved {
  from = terraform_data.api
  to   = terraform_data.web
}
moved {
  from = terraform_data.older
  to   = terraform_data.other
}
removed {
  from = terraform_data.retired
  lifecycle {
    destroy = false
  }
}
`
	testCases := []struct {
		newName string
		wantErr string
	}{
		{"api", "the moved block at main.tf:17 moves terraform_data.api to terraform_data.web, so renaming back would make a cycle; delete that moved block first if it has not been applied, or pick another name"},
		{"older", "terraform_data.older is the from address of a moved block (main.tf:21), so OpenTofu would move the renamed object; pick another name"},
		{"retired", "terraform_data.retired is the from address of a removed block (main.tf:25), so OpenTofu would remove the renamed object; pick another name"},
	}
	for _, tc := range testCases {
		t.Run(tc.newName, func(t *testing.T) {
			ls, tmpDir, stop := startNavigationServer(t, map[string]string{
				"main.tf": navigationMainTf + history,
			})
			defer stop()

			ls.CallAndExpectError(t, &langserver.CallRequest{
				Method: "textDocument/rename",
				ReqParams: fmt.Sprintf(`{
					"textDocument": {"uri": "%s/main.tf"},
					"position": {"line": 8, "character": 28},
					"newName": %q
				}`, tmpDir.URI, tc.newName)}, &jrpc2.Error{Code: jrpc2.Code(-32098), Message: tc.wantErr})
		})
	}
}

func TestRename_overrideAndTestFiles(t *testing.T) {
	files := map[string]string{
		"main.tf":     navigationMainTf,
		"override.tf": "resource \"terraform_data\" \"web\" {\n  input = \"x\"\n}\n",
		"tests/main.tftest.hcl": `variables {
  stage = "test"
}
run "check" {
  command = plan
  variables {
    stage = "dev"
  }
  assert {
    condition     = terraform_data.web.input == "app-dev" && var.stage == "dev"
    error_message = "wrong input"
  }
}
`,
	}
	ls, tmpDir, stop := startNavigationServerWithOptions(t, files, `{"rename": {"addMovedBlock": false}}`)
	defer stop()

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 8, "character": 28},
			"newName": "api"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 8, "character": 27}, "end": {"line": 8, "character": 30}}, "newText": "api"},
						{"range": {"start": {"line": 13, "character": 25}, "end": {"line": 13, "character": 28}}, "newText": "api"}
					],
					"%s/override.tf": [
						{"range": {"start": {"line": 0, "character": 27}, "end": {"line": 0, "character": 30}}, "newText": "api"}
					],
					"%s/tests/main.tftest.hcl": [
						{"range": {"start": {"line": 9, "character": 35}, "end": {"line": 9, "character": 38}}, "newText": "api"}
					]
				}
			}
		}`, tmpDir.URI, tmpDir.URI, tmpDir.URI))

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12},
			"newName": "tier"
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 4,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 0, "character": 10}, "end": {"line": 0, "character": 15}}, "newText": "tier"},
						{"range": {"start": {"line": 5, "character": 20}, "end": {"line": 5, "character": 25}}, "newText": "tier"}
					],
					"%s/tests/main.tftest.hcl": [
						{"range": {"start": {"line": 1, "character": 2}, "end": {"line": 1, "character": 7}}, "newText": "tier"},
						{"range": {"start": {"line": 6, "character": 4}, "end": {"line": 6, "character": 9}}, "newText": "tier"},
						{"range": {"start": {"line": 9, "character": 65}, "end": {"line": 9, "character": 70}}, "newText": "tier"}
					]
				}
			}
		}`, tmpDir.URI, tmpDir.URI))
}

func TestRename_refusesModuleOutsideWorkspace(t *testing.T) {
	// the module lives next to the workspace folder, as in a monorepo
	// opened at envs/prod: other callers of it cannot be seen
	shared := filepath.Join(os.TempDir(), "tofu-ls", t.Name()+"-shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shared) })
	if err := os.WriteFile(filepath.Join(shared, "variables.tf"), []byte("variable \"replicas\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": fmt.Sprintf("module \"app\" {\n  source   = \"../%s\"\n  replicas = 1\n}\n", filepath.Base(shared)),
	})
	defer stop()

	ls.CallAndExpectError(t, &langserver.CallRequest{
		Method: "textDocument/prepareRename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 2, "character": 4}
		}`, tmpDir.URI)}, &jrpc2.Error{Code: jrpc2.Code(-32098), Message: fmt.Sprintf("var.replicas is declared in %s, outside the workspace, where other callers of the module cannot be seen; open that folder to rename it", shared)})
}

func TestRename_checkScopedData(t *testing.T) {
	ls, tmpDir, stop := startNavigationServer(t, map[string]string{
		"main.tf": `check "motd" {
  data "local_file" "check_motd" {
    filename = "motd.txt"
  }
  assert {
    condition     = data.local_file.check_motd.content != ""
    error_message = "empty"
  }
}
`,
	})
	defer stop()

	want := fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": %%d,
			"result": {
				"changes": {
					"%s/main.tf": [
						{"range": {"start": {"line": 1, "character": 21}, "end": {"line": 1, "character": 31}}, "newText": "motd_file"},
						{"range": {"start": {"line": 5, "character": 36}, "end": {"line": 5, "character": 46}}, "newText": "motd_file"}
					]
				}
			}
		}`, tmpDir.URI)
	// from the reference in the assertion
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 5, "character": 38},
			"newName": "motd_file"
		}`, tmpDir.URI)}, fmt.Sprintf(want, 3))
	// from the label of the declaration
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 1, "character": 25},
			"newName": "motd_file"
		}`, tmpDir.URI)}, fmt.Sprintf(want, 4))
}
