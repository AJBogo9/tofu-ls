// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"fmt"
	"testing"

	"github.com/creachadair/jrpc2"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/langserver/cmd"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

func TestLangServer_workspaceExecuteCommand_moduleGraph_argumentError(t *testing.T) {
	tmpDir := TempDir(t)
	InitPluginCache(t, tmpDir.Path())

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	wc := walker.NewWalkerCollector()

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				tmpDir.Path(): validTfMockCalls(),
			},
		},
		StateStore:      ss,
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {},
	    "rootUri": %q,
		"processId": 12345
	}`, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})

	ls.CallAndExpectError(t, &langserver.CallRequest{
		Method: "workspace/executeCommand",
		ReqParams: fmt.Sprintf(`{
		"command": %q
	}`, cmd.Name("module.graph"))}, jrpc2.InvalidParams.Err())
}

func TestLangServer_workspaceExecuteCommand_moduleGraph_basic(t *testing.T) {
	tmpDir := TempDir(t)
	testFileURI := fmt.Sprintf("%s/main.tf", tmpDir.URI)
	InitPluginCache(t, tmpDir.Path())

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	wc := walker.NewWalkerCollector()

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				tmpDir.Path(): validTfMockCalls(),
			},
		},
		StateStore:      ss,
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {},
	    "rootUri": %q,
		"processId": 12345
	}`, tmpDir.URI)})
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
			"text": "variable \"name\" {\n  description = \"Who to greet\"\n}\n\nlocals {\n  greeting = \"hi ${var.name}\"\n}\n\noutput \"greeting\" {\n  value = local.greeting\n}\n",
			"uri": %q
		}
	}`, testFileURI)})
	waitForAllJobs(t, ss)

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "workspace/executeCommand",
		ReqParams: fmt.Sprintf(`{
		"command": %q,
		"arguments": ["uri=%s"]
	}`, cmd.Name("module.graph"), tmpDir.URI)}, fmt.Sprintf(`{
		"jsonrpc": "2.0",
		"id": 3,
		"result": {
			"v": 0,
			"module_uri": %q,
			"nodes": [
				{
					"id": "var.name",
					"kind": "variable",
					"name": "name",
					"uri": %q,
					"range": {"start": {"line": 0, "character": 0}, "end": {"line": 2, "character": 1}},
					"name_range": {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 15}},
					"description": "Who to greet"
				},
				{
					"id": "local.greeting",
					"kind": "local",
					"name": "greeting",
					"uri": %q,
					"range": {"start": {"line": 5, "character": 2}, "end": {"line": 5, "character": 29}},
					"name_range": {"start": {"line": 5, "character": 2}, "end": {"line": 5, "character": 10}},
					"detail": "\"hi ${var.name}\""
				},
				{
					"id": "output.greeting",
					"kind": "output",
					"name": "greeting",
					"uri": %q,
					"range": {"start": {"line": 8, "character": 0}, "end": {"line": 10, "character": 1}},
					"name_range": {"start": {"line": 8, "character": 0}, "end": {"line": 8, "character": 17}}
				}
			],
			"edges": [
				{
					"from": "local.greeting",
					"to": "output.greeting",
					"refs": [
						{
							"uri": %q,
							"range": {"start": {"line": 9, "character": 10}, "end": {"line": 9, "character": 24}},
							"attribute": "value",
							"text": "local.greeting"
						}
					]
				},
				{
					"from": "var.name",
					"to": "local.greeting",
					"refs": [
						{
							"uri": %q,
							"range": {"start": {"line": 5, "character": 19}, "end": {"line": 5, "character": 27}},
							"text": "var.name"
						}
					]
				}
			]
		}
	}`, tmpDir.URI, testFileURI, testFileURI, testFileURI, testFileURI, testFileURI))
}
