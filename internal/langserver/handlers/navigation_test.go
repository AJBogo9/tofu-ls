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
	tmpDir := TempDir(t)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tmpDir.Path(), name), []byte(content), 0o644); err != nil {
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
