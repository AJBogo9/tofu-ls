// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/go-version"
	"github.com/opentofu/tofu-ls/internal/langserver"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

// rangeFormattingMessy and its tofu fmt output, taken from tofu 1.12.
const rangeFormattingMessy = `resource "a" "b" {
  x=1
    yy = 2
}

resource   "c" "d"   {
foo="bar"
  list = [1,2,
  3]
}

# comment
variable "v" {
default=1
    type = string
}
`

const rangeFormattingFormatted = `resource "a" "b" {
  x  = 1
  yy = 2
}

resource "c" "d" {
  foo = "bar"
  list = [1, 2,
  3]
}

# comment
variable "v" {
  default = 1
  type    = string
}
`

// applyLSPEdits applies edits to an ASCII text.
func applyLSPEdits(t *testing.T, text string, edits []lsp.TextEdit) string {
	t.Helper()
	lines := strings.SplitAfter(text, "\n")
	offset := func(p lsp.Position) int {
		o := 0
		for _, l := range lines[:p.Line] {
			o += len(l)
		}
		return o + int(p.Character)
	}
	// apply from the end, so that earlier offsets stay valid
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		text = text[:offset(e.Range.Start)] + e.NewText + text[offset(e.Range.End):]
	}
	return text
}

func TestLangServer_rangeFormatting(t *testing.T) {
	tmpDir := TempDir(t)

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	wc := walker.NewWalkerCollector()

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore:      ss,
		WalkerCollector: wc,
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				tmpDir.Path(): {
					{
						Method:          "Version",
						Repeatability:   1,
						Arguments:       []interface{}{mock.AnythingOfType("")},
						ReturnArguments: []interface{}{version.Must(version.NewVersion("1.12.0")), nil, nil},
					},
					{
						Method:          "GetExecPath",
						ReturnArguments: []interface{}{""},
					},
					{
						Method: "Format",
						Arguments: []interface{}{
							mock.AnythingOfType(""),
							[]byte(rangeFormattingMessy),
						},
						ReturnArguments: []interface{}{[]byte(rangeFormattingFormatted), nil},
					},
				},
			},
		},
	}))
	stop := ls.Start(t)
	defer stop()

	initResp := ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {},
	    "rootUri": %q,
	    "processId": 12345
	}`, tmpDir.URI)})
	var initResult lsp.InitializeResult
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatal(err)
	}
	if !initResult.Capabilities.DocumentRangeFormattingProvider {
		t.Fatal("range formatting is not advertised")
	}
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	text, _ := json.Marshal(rangeFormattingMessy)
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

	testCases := []struct {
		name  string
		rng   string
		want  string
		lines [2]uint32
	}{
		{
			"a line inside the second block",
			`{"start": {"line": 6, "character": 2}, "end": {"line": 6, "character": 4}}`,
			`resource "a" "b" {
  x=1
    yy = 2
}

resource "c" "d" {
  foo = "bar"
  list = [1, 2,
  3]
}

# comment
variable "v" {
default=1
    type = string
}
`,
			[2]uint32{5, 9},
		},
		{
			"whole lines of the last block, ending at the start of the next line",
			`{"start": {"line": 12, "character": 0}, "end": {"line": 16, "character": 0}}`,
			`resource "a" "b" {
  x=1
    yy = 2
}

resource   "c" "d"   {
foo="bar"
  list = [1,2,
  3]
}

# comment
variable "v" {
  default = 1
  type    = string
}
`,
			[2]uint32{12, 16},
		},
		{
			"between blocks",
			`{"start": {"line": 10, "character": 0}, "end": {"line": 10, "character": 0}}`,
			rangeFormattingMessy,
			[2]uint32{0, 0},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rsp := ls.Call(t, &langserver.CallRequest{
				Method: "textDocument/rangeFormatting",
				ReqParams: fmt.Sprintf(`{
				"textDocument": {"uri": "%s/main.tf"},
				"range": %s,
				"options": {"tabSize": 2, "insertSpaces": true}
			}`, tmpDir.URI, tc.rng)})
			var edits []lsp.TextEdit
			if err := json.Unmarshal(rsp.Result, &edits); err != nil {
				t.Fatal(err)
			}
			for _, e := range edits {
				if e.Range.Start.Line < tc.lines[0] || e.Range.End.Line > tc.lines[1] {
					t.Fatalf("edit outside of the formatted blocks: %#v", e)
				}
			}
			if diff := cmp.Diff(tc.want, applyLSPEdits(t, rangeFormattingMessy, edits)); diff != "" {
				t.Fatalf("unexpected result: %s", diff)
			}
		})
	}
}
