// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/opentofu/tofu-ls/internal/langserver"
)

// TestValuesInputs_vars gives static values the -var options of the plan
// arguments with tofu-ls.values.inputs: they apply in their command-line
// order among the -var-file files, after the automatically loaded ones.
func TestValuesInputs_vars(t *testing.T) {
	ls, tmpDir, stop := startStaticValuesServer(t, `{}`)
	defer stop()
	dirURI := tmpDir.URI
	if err := os.MkdirAll(filepath.Join(tmpDir.Path(), "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir.Path(), "envs", "qa.tfvars"), []byte("stage = \"qa\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := 2
	hints := func(stage string) {
		id++
		ls.CallAndExpectResponse(t, &langserver.CallRequest{
			Method: "textDocument/inlayHint",
			ReqParams: fmt.Sprintf(`{
				"textDocument": {"uri": "%s/main.tf"},
				"range": {"start": {"line": 0, "character": 0}, "end": {"line": 13, "character": 0}}
			}`, dirURI)}, fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": %d,
				"result": [
					{"position": {"line": 7, "character": 27}, "label": [{"value": "▸ \"app-%s\""}], "paddingLeft": true},
					{"position": {"line": 11, "character": 20}, "label": [{"value": "▸ \"app-%s\""}], "paddingLeft": true}
				]
			}`, id, stage, stage))
	}
	choose := func(varFiles, vars string) {
		id++
		args := []string{"varFiles=" + varFiles, "vars=" + vars}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		ls.CallAndExpectResponse(t, &langserver.CallRequest{
			Method:    "workspace/executeCommand",
			ReqParams: fmt.Sprintf(`{"command": "tofu-ls.values.inputs", "arguments": %s}`, raw),
		}, fmt.Sprintf(`{"jsonrpc": "2.0", "id": %d, "result": null}`, id))
	}

	// above terraform.tfvars
	choose(`{}`, fmt.Sprintf(`{%q: [{"raw": "stage=cli", "after": 0}]}`, dirURI))
	hints("cli")

	// after the chosen file: the -var wins, and the hover names it
	choose(fmt.Sprintf(`{%q: ["envs/qa.tfvars"]}`, dirURI), fmt.Sprintf(`{%q: [{"raw": "stage=cli", "after": 1}]}`, dirURI))
	hints("cli")
	content, err := json.Marshal("`var.stage` _string_\n\n" +
		"Deployment stage.\n\n" +
		"**Value** `\"cli\"` from `-var stage` (plan arguments)\n\n" +
		"Overrides: `envs/qa.tfvars` (selected environment) sets `\"qa\"`; `terraform.tfvars` sets `\"prod\"`; default `\"dev\"`\n\n" +
		"_May be overridden by `-var` or `-var-file`._\n\n" +
		"_Declared in `main.tf`_")
	if err != nil {
		t.Fatal(err)
	}
	id++
	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/hover",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 7, "character": 20}
		}`, dirURI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": %d,
			"result": {
				"contents": {"kind": "markdown", "value": %s},
				"range": {"start": {"line": 7, "character": 16}, "end": {"line": 7, "character": 25}}
			}
		}`, id, content))

	// before the chosen file: the file wins
	choose(fmt.Sprintf(`{%q: ["envs/qa.tfvars"]}`, dirURI), fmt.Sprintf(`{%q: [{"raw": "stage=cli", "after": 0}]}`, dirURI))
	hints("qa")

	// none
	choose(`{}`, `{}`)
	hints("prod")
}
