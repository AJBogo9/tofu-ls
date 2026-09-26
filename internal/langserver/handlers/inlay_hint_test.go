// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-version"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

const staticValuesConfig = `variable "stage" {
  description = "Deployment stage."
  type        = string
  default     = "dev"
}

locals {
  name = "app-${var.stage}"
}

output "n" {
  value = local.name
}
`

// responseChecker is the part of the language server mock the tests use.
type responseChecker interface {
	CallAndExpectResponse(t *testing.T, cr *langserver.CallRequest, expectRaw string)
}

// startStaticValuesServer starts a server on a module whose terraform.tfvars
// overrides the default of var.stage, and opens main.tf.
func startStaticValuesServer(t *testing.T, initOptions string) (responseChecker, document.DirHandle, func()) {
	tmpDir := TempDir(t)
	InitPluginCache(t, tmpDir.Path())
	if err := os.WriteFile(filepath.Join(tmpDir.Path(), "terraform.tfvars"), []byte("stage = \"prod\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var testSchema tfjson.ProviderSchemas
	if err := json.Unmarshal([]byte(testModuleSchemaOutput), &testSchema); err != nil {
		t.Fatal(err)
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
						Arguments:     []interface{}{mock.AnythingOfType("")},
						ReturnArguments: []interface{}{
							version.Must(version.NewVersion("1.9.0")),
							nil,
							nil,
						},
					},
					{
						Method:          "GetExecPath",
						Repeatability:   1,
						ReturnArguments: []interface{}{""},
					},
					{
						Method:          "ProviderSchemas",
						Repeatability:   1,
						Arguments:       []interface{}{mock.AnythingOfType("")},
						ReturnArguments: []interface{}{&testSchema, nil},
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
		"capabilities": {"textDocument": {"hover": {"contentFormat": ["markdown"]}}},
		"initializationOptions": %s,
		"rootUri": %q,
		"processId": 12345
	}`, initOptions, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	text, err := json.Marshal(staticValuesConfig)
	if err != nil {
		t.Fatal(err)
	}
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "opentofu",
			"text": %s,
			"uri": "%s/main.tf"
		}
	}`, text, tmpDir.URI)})
	waitForAllJobs(t, ss)

	return ls, tmpDir, func() { stop() }
}

func TestInlayHint_values(t *testing.T) {
	testCases := []struct {
		name        string
		initOptions string
		expected    string
	}{
		{
			"informative by default: the local's value instead of a hint inside its template",
			`{}`,
			`[
				{"position": {"line": 7, "character": 27}, "label": [{"value": "▸ \"app-prod\""}], "paddingLeft": true},
				{"position": {"line": 11, "character": 20}, "label": [{"value": "▸ \"app-prod\""}], "paddingLeft": true}
			]`,
		},
		{
			"all",
			`{"inlayHints": {"valuePolicy": "all"}}`,
			`[
				{"position": {"line": 7, "character": 25}, "label": [{"value": "▸ \"prod\""}], "paddingLeft": true},
				{"position": {"line": 11, "character": 20}, "label": [{"value": "▸ \"app-prod\""}], "paddingLeft": true}
			]`,
		},
		{
			"max length",
			`{"inlayHints": {"maxLength": 6, "valuePolicy": "all"}}`,
			`[
				{"position": {"line": 7, "character": 25}, "label": [{"value": "▸ \"prod\""}], "paddingLeft": true},
				{"position": {"line": 11, "character": 20}, "label": [{"value": "▸ \"app…\""}], "paddingLeft": true}
			]`,
		},
		{
			"disabled",
			`{"inlayHints": {"values": false}}`,
			`[]`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ls, tmpDir, stop := startStaticValuesServer(t, tc.initOptions)
			defer stop()

			ls.CallAndExpectResponse(t, &langserver.CallRequest{
				Method: "textDocument/inlayHint",
				ReqParams: fmt.Sprintf(`{
				"textDocument": {"uri": "%s/main.tf"},
				"range": {"start": {"line": 0, "character": 0}, "end": {"line": 13, "character": 0}}
			}`, tmpDir.URI)}, fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": 3,
				"result": %s
			}`, tc.expected))
		})
	}
}

func TestHover_variableValue(t *testing.T) {
	ls, tmpDir, stop := startStaticValuesServer(t, `{}`)
	defer stop()

	content, err := json.Marshal("`var.stage` _string_\n\n" +
		"Deployment stage.\n\n" +
		"**Value** `\"prod\"` from `terraform.tfvars`\n\n" +
		"Overrides: default `\"dev\"`\n\n" +
		"_May be overridden by `-var` or `-var-file`._\n\n" +
		"_Declared in `main.tf`_")
	if err != nil {
		t.Fatal(err)
	}
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/hover",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 7, "character": 20}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": {
				"contents": {"kind": "markdown", "value": %s},
				"range": {"start": {"line": 7, "character": 16}, "end": {"line": 7, "character": 25}}
			}
		}`, content))
}
