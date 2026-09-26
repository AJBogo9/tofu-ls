// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/go-version"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

const testFilesMainTf = `variable "prefix" {
  type        = string
  default     = "demo"
  description = "Name prefix"
}

locals {
  label = "${var.prefix}-label"
}

resource "terraform_data" "echo" {
  input = local.label
}

output "label" {
  value = local.label
}
`

const testFilesChildTf = `variable "name" {
  type = string
}

output "greeting" {
  value = "hello ${var.name}"
}
`

// the positions below are 0-based (line, character), as in LSP
const testFilesTest = `variables {
  prefix = "unit"
}

run "first" {
  assert {
    condition     = local.label == var.prefix
    error_message = "x"
  }
}

run "child" {
  module {
    source = "./modules/child"
  }

  variables {
    name = run.first.label
  }

  assert {
    condition     = output.greeting == var.name
    error_message = "y"
  }
}
`

// testFilesCall sends a request and returns its result.
type testFilesCall func(t *testing.T, method, params string) json.RawMessage

// startTestFilesServer writes a module with a child module and a test
// file into a temporary directory, opens main.tf and the test file, and
// waits until everything is indexed.
func startTestFilesServer(t *testing.T) (testFilesCall, document.DirHandle, func()) {
	tmpDir := TempDir(t)
	files := map[string]string{
		"main.tf":               testFilesMainTf,
		"modules/child/main.tf": testFilesChildTf,
		"tests/a.tftest.hcl":    testFilesTest,
	}
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
	versionCalls := []*mock.Call{
		{
			Method:        "Version",
			Repeatability: 1,
			Arguments:     []interface{}{mock.AnythingOfType("")},
			ReturnArguments: []interface{}{
				version.Must(version.NewVersion("1.12.0")),
				nil,
				nil,
			},
		},
		{
			Method:          "GetExecPath",
			Repeatability:   1,
			ReturnArguments: []interface{}{""},
		},
	}
	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				tmpDir.Path(): versionCalls,
				filepath.Join(tmpDir.Path(), "modules", "child"): versionCalls,
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
	    "rootUri": %q,
	    "processId": 12345
	}`, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	for _, open := range []struct{ name, languageID string }{
		{"main.tf", "opentofu"},
		{"tests/a.tftest.hcl", "opentofu-test"},
	} {
		ls.Call(t, &langserver.CallRequest{
			Method: "textDocument/didOpen",
			ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": %q,
			"text": %q,
			"uri": "%s/%s"
		}
	}`, open.languageID, files[open.name], tmpDir.URI, open.name)})
	}
	waitForAllJobs(t, ss)

	call := func(t *testing.T, method, params string) json.RawMessage {
		return ls.Call(t, &langserver.CallRequest{Method: method, ReqParams: params}).Result
	}
	return call, tmpDir, stop
}

func positionParams(dir document.DirHandle, file string, line, character int) string {
	return fmt.Sprintf(`{
		"textDocument": {"uri": "%s/%s"},
		"position": {"line": %d, "character": %d}
	}`, dir.URI, file, line, character)
}

type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct{ Line, Character int }
		End   struct{ Line, Character int }
	} `json:"range"`
}

func (l lspLocation) String(dir document.DirHandle) string {
	return fmt.Sprintf("%s:%d", strings.TrimPrefix(l.URI, dir.URI+"/"), l.Range.Start.Line)
}

func TestTestFiles_definition(t *testing.T) {
	call, tmpDir, stop := startTestFilesServer(t)
	defer stop()

	testCases := []struct {
		name      string
		line, col int
		want      []string
	}{
		{"var of the root module in an assert", 6, 38, []string{"main.tf:0"}},
		{"local of the root module in an assert", 6, 26, []string{"main.tf:7"}},
		{"key of the file's variables", 1, 4, []string{"main.tf:0"}},
		{"output of the child module", 21, 28, []string{"modules/child/main.tf:4"}},
		{"var of the child module", 21, 44, []string{"modules/child/main.tf:0"}},
		{"key of the child run's variables", 17, 5, []string{"modules/child/main.tf:0"}},
		{"output of an earlier run", 17, 22, []string{"tests/a.tftest.hcl:4"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := call(t, "textDocument/definition", positionParams(tmpDir, "tests/a.tftest.hcl", tc.line, tc.col))
			var locations []lspLocation
			if err := json.Unmarshal(result, &locations); err != nil {
				t.Fatalf("%s: %s", err, result)
			}
			got := make([]string, 0)
			for _, l := range locations {
				got = append(got, l.String(tmpDir))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected definitions: %s", diff)
			}
		})
	}
}

func TestTestFiles_hover(t *testing.T) {
	call, tmpDir, stop := startTestFilesServer(t)
	defer stop()

	testCases := []struct {
		name      string
		line, col int
		contains  []string
	}{
		{"var of the root module", 6, 38, []string{"`var.prefix`", "_string_", "Name prefix", "the module under test (the root module"}},
		{"var of the child module", 21, 44, []string{"`var.name`", "module `./modules/child`"}},
		{"output of the child module", 21, 28, []string{"`output.greeting`"}},
		{"run block", 4, 1, []string{"**run** _Block_", "The runs of a file run in order"}},
		{"assert condition", 6, 6, []string{"**condition** _required, bool_"}},
		{"key of the child run's variables", 17, 5, []string{"**name** _optional, string_"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := call(t, "textDocument/hover", positionParams(tmpDir, "tests/a.tftest.hcl", tc.line, tc.col))
			var hover struct {
				Contents struct{ Value string } `json:"contents"`
			}
			if err := json.Unmarshal(result, &hover); err != nil {
				t.Fatalf("%s: %s", err, result)
			}
			for _, want := range tc.contains {
				if !strings.Contains(hover.Contents.Value, want) {
					t.Errorf("hover lacks %q:\n%s", want, hover.Contents.Value)
				}
			}
		})
	}
}

func completionLabels(t *testing.T, call testFilesCall, dir document.DirHandle, line, col int) []string {
	result := call(t, "textDocument/completion", positionParams(dir, "tests/a.tftest.hcl", line, col))
	var list struct {
		Items []struct{ Label string } `json:"items"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatalf("%s: %s", err, result)
	}
	labels := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		labels = append(labels, item.Label)
	}
	sort.Strings(labels)
	return labels
}

func TestTestFiles_completion(t *testing.T) {
	call, tmpDir, stop := startTestFilesServer(t)
	defer stop()

	has := func(labels []string, label string) bool {
		for _, l := range labels {
			if l == label {
				return true
			}
		}
		return false
	}

	testCases := []struct {
		name      string
		line, col int
		want      []string
		notWant   []string
	}{
		// after "var." in "var.prefix"
		{"vars of the root in a root run", 6, 39, []string{"var.prefix"}, []string{"var.name"}},
		// after "var." in "var.name"
		{"vars of the child in a child run", 21, 43, []string{"var.name"}, []string{"var.prefix"}},
		// after "output." in "output.greeting"
		{"outputs of the child in a child run", 21, 27, []string{"output.greeting"}, []string{"output.label"}},
		// after "run.first." in the variables of the later run
		{"outputs of an earlier run", 17, 21, []string{"run.first.label"}, nil},
		// on an empty line of the file's body, which has its one variables block
		{"blocks of a test file", 3, 0, []string{"mock_provider", "override_resource", "provider", "run"}, []string{"test", "variables"}},
		// on the empty line between the module and variables blocks of a run
		{"blocks and arguments of a run", 15, 2, []string{"assert", "command", "expect_failures", "plan_options"}, []string{"module", "variables"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			labels := completionLabels(t, call, tmpDir, tc.line, tc.col)
			for _, want := range tc.want {
				if !has(labels, want) {
					t.Errorf("missing %q in %q", want, labels)
				}
			}
			for _, notWant := range tc.notWant {
				if has(labels, notWant) {
					t.Errorf("unexpected %q in %q", notWant, labels)
				}
			}
		})
	}
}

func TestTestFiles_referencesFromModule(t *testing.T) {
	call, tmpDir, stop := startTestFilesServer(t)
	defer stop()

	// on the "prefix" label of variable "prefix" in main.tf
	result := call(t, "textDocument/references", fmt.Sprintf(`{
			"textDocument": {"uri": "%s/main.tf"},
			"position": {"line": 0, "character": 12},
			"context": {"includeDeclaration": false}
		}`, tmpDir.URI))
	var locations []lspLocation
	if err := json.Unmarshal(result, &locations); err != nil {
		t.Fatalf("%s: %s", err, result)
	}
	got := make([]string, 0)
	for _, l := range locations {
		got = append(got, l.String(tmpDir))
	}
	sort.Strings(got)
	// the use in the module, the key of the file's variables and the use
	// in the assert of the run of the root module; not the child run's
	want := []string{"main.tf:7", "tests/a.tftest.hcl:1", "tests/a.tftest.hcl:6"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected references: %s", diff)
	}
}

func TestTestFiles_documentSymbols(t *testing.T) {
	call, tmpDir, stop := startTestFilesServer(t)
	defer stop()

	result := call(t, "textDocument/documentSymbol", fmt.Sprintf(`{"textDocument": {"uri": "%s/tests/a.tftest.hcl"}}`, tmpDir.URI))
	var symbols []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result, &symbols); err != nil {
		t.Fatalf("%s: %s", err, result)
	}
	got := make([]string, 0)
	for _, s := range symbols {
		got = append(got, s.Name)
	}
	want := []string{"variables", `run "first"`, `run "child"`}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected symbols: %s", diff)
	}
}
