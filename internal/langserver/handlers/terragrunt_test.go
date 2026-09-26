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
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

// the positions below are 0-based (line, character), as in LSP
const terragruntUnit = `include "root" {
  path = find_in_parent_folders("root.hcl")
}

include "common" {
  path   = "${dirname(find_in_parent_folders("root.hcl"))}/common/app.hcl"
  expose = true
}

locals {
  name = "app"
}

dependency "vpc" {
  config_path = "../vpc"
}

terraform {
  source = "../../modules//vpc"
}

inputs = {
  vpc_id  = dependency.vpc.outputs.vpc_id
  name    = local.name
  service = include.common.locals.service
  other   = local.n
  first   = fi
}

`

const terragruntRoot = `locals {
  region = "eu-north-1"
}

remote_state {
  backend = "s3"
  config = {
    region = local.region
  }
}
`

func startTerragruntServer(t *testing.T) (testFilesCall, document.DirHandle, func()) {
	tmpDir := TempDir(t)
	files := map[string]string{
		"root.hcl":                terragruntRoot,
		"common/app.hcl":          "locals {\n  service = \"api\"\n}\n",
		"modules/vpc/main.tf":     "variable \"cidr\" {}\n",
		"live/app/terragrunt.hcl": terragruntUnit,
		"live/vpc/terragrunt.hcl": "terraform {\n  source = \"../../modules//vpc\"\n}\n",
		"live/app/account.hcl":    "locals {}\n",
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
			Method:          "Version",
			Repeatability:   1,
			Arguments:       []interface{}{mock.AnythingOfType("")},
			ReturnArguments: []interface{}{nil, nil, fmt.Errorf("no tofu here")},
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
				filepath.Join(tmpDir.Path(), "modules", "vpc"): versionCalls,
			},
		},
		StateStore:      ss,
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {
	      "textDocument": {"hover": {"contentFormat": ["markdown"]}},
	      "experimental": {"showReferencesCommandId": "client.showReferences"}
	    },
	    "rootUri": %q,
	    "processId": 12345
	}`, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	for _, open := range []struct{ name, languageID string }{
		{"live/app/terragrunt.hcl", "terragrunt"},
		{"root.hcl", "terragrunt"},
		{"modules/vpc/main.tf", "opentofu"},
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

func TestTerragrunt_definition(t *testing.T) {
	call, tmpDir, stop := startTerragruntServer(t)
	defer stop()

	testCases := []struct {
		name      string
		file      string
		line, col int
		want      []string
	}{
		{"local", "live/app/terragrunt.hcl", 23, 18, []string{"live/app/terragrunt.hcl:10"}},
		{"dependency outputs", "live/app/terragrunt.hcl", 22, 30, []string{"live/app/terragrunt.hcl:13"}},
		{"local of an exposed include", "live/app/terragrunt.hcl", 24, 36, []string{"common/app.hcl:1"}},
		{"include path from find_in_parent_folders", "live/app/terragrunt.hcl", 1, 12, []string{"root.hcl:0"}},
		{"include path built with dirname", "live/app/terragrunt.hcl", 5, 60, []string{"common/app.hcl:0"}},
		{"dependency config_path", "live/app/terragrunt.hcl", 14, 18, []string{"live/vpc/terragrunt.hcl:0"}},
		{"local source", "live/app/terragrunt.hcl", 18, 15, []string{"modules/vpc/main.tf:0"}},
		{"local in root.hcl", "root.hcl", 7, 17, []string{"root.hcl:1"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := call(t, "textDocument/definition", positionParams(tmpDir, tc.file, tc.line, tc.col))
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

func TestTerragrunt_documentLink(t *testing.T) {
	call, tmpDir, stop := startTerragruntServer(t)
	defer stop()

	links := func(file string) []string {
		result := call(t, "textDocument/documentLink", fmt.Sprintf(`{"textDocument": {"uri": "%s/%s"}}`, tmpDir.URI, file))
		var list []struct {
			Range struct {
				Start struct{ Line, Character int }
			}
			Target string `json:"target"`
		}
		if err := json.Unmarshal(result, &list); err != nil {
			t.Fatalf("%s: %s", err, result)
		}
		got := make([]string, 0)
		for _, l := range list {
			got = append(got, fmt.Sprintf("%d:%d %s", l.Range.Start.Line, l.Range.Start.Character,
				strings.TrimPrefix(l.Target, tmpDir.URI+"/")))
		}
		return got
	}

	want := []string{
		"1:9 root.hcl",
		"5:12 common/app.hcl",
		"14:17 live/vpc/terragrunt.hcl",
		"18:12 modules/vpc/main.tf",
	}
	if diff := cmp.Diff(want, links("live/app/terragrunt.hcl")); diff != "" {
		t.Errorf("unit links: %s", diff)
	}
	if got := links("root.hcl"); len(got) != 0 {
		t.Errorf("root.hcl is included, so its paths depend on the unit: got %v", got)
	}
}

func TestTerragrunt_hoverAndCompletion(t *testing.T) {
	call, tmpDir, stop := startTerragruntServer(t)
	defer stop()

	hovers := []struct {
		name      string
		line, col int
		contains  []string
	}{
		{"block", 13, 2, []string{"**dependency** _Block_", "docs.terragrunt.com"}},
		{"function", 1, 12, []string{"find_in_parent_folders(name string, fallback string) string"}},
		{"dependency reference", 22, 25, []string{"`dependency.vpc`", "Terragrunt reads from its state"}},
		{"include local", 24, 36, []string{"Local value `service` of `../../common/app.hcl`"}},
	}
	for _, tc := range hovers {
		t.Run("hover "+tc.name, func(t *testing.T) {
			result := call(t, "textDocument/hover", positionParams(tmpDir, "live/app/terragrunt.hcl", tc.line, tc.col))
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

	completion := func(line, col int) []string {
		result := call(t, "textDocument/completion", positionParams(tmpDir, "live/app/terragrunt.hcl", line, col))
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
	// the empty last line: blocks and attributes the file has no more room for are left out
	top := completion(28, 0)
	for _, want := range []string{"dependency", "feature", "generate", "remote_state", "prevent_destroy", "catalog"} {
		if !contains(top, want) {
			t.Errorf("top level: missing %q in %q", want, top)
		}
	}
	for _, notWant := range []string{"terraform", "inputs", "resource", "variable"} {
		if contains(top, notWant) {
			t.Errorf("top level: unexpected %q in %q", notWant, top)
		}
	}
	// a reference in a top-level attribute, after "local.n"
	if diff := cmp.Diff([]string{"local.name"}, completion(25, 19)); diff != "" {
		t.Errorf("local references: %s", diff)
	}
	// Terragrunt and OpenTofu functions, after "fi"
	functions := completion(26, 14)
	for _, want := range []string{"find_in_parent_folders", "file", "fileexists"} {
		if !contains(functions, want) {
			t.Errorf("functions: missing %q in %q", want, functions)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestTerragrunt_notRenamable(t *testing.T) {
	call, tmpDir, stop := startTerragruntServer(t)
	defer stop()

	result := call(t, "textDocument/prepareRename", positionParams(tmpDir, "live/app/terragrunt.hcl", 10, 3))
	if string(result) != "null" {
		t.Errorf("a Terragrunt local is not renamable yet, got %s", result)
	}
}

// Other Terragrunt files read a file's values (include with expose,
// read_terragrunt_config), so its uses cannot be counted from the file.
func TestTerragrunt_noReferenceCountLenses(t *testing.T) {
	call, tmpDir, stop := startTerragruntServer(t)
	defer stop()

	lenses := func(file string) int {
		result := call(t, "textDocument/codeLens", fmt.Sprintf(`{"textDocument": {"uri": "%s/%s"}}`, tmpDir.URI, file))
		var list []json.RawMessage
		if err := json.Unmarshal(result, &list); err != nil {
			t.Fatalf("%s: %s", err, result)
		}
		return len(list)
	}
	// the control: the module's variable gets its lens
	if n := lenses("modules/vpc/main.tf"); n == 0 {
		t.Fatal("expected a reference count lens in main.tf")
	}
	for _, file := range []string{"live/app/terragrunt.hcl", "root.hcl"} {
		if n := lenses(file); n != 0 {
			t.Errorf("%s: expected no lenses, got %d", file, n)
		}
	}
}
