// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/uri"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

// providerCompletionSession starts a server on dir, with the test provider
// schema installed, opens every .tf file of dir and returns a function that
// requests completion.
func providerCompletionSession(t *testing.T, dir string, initOptions string, clientCaps string) func(file string, line, character int) lsp.CompletionList {
	t.Helper()
	request := testSession(t, dir, initOptions, clientCaps)
	return func(file string, line, character int) lsp.CompletionList {
		result, err := request("textDocument/completion", fmt.Sprintf(`{
	"textDocument": {"uri": %q},
	"position": {"line": %d, "character": %d}
}`, file, line, character))
		if err != nil {
			t.Fatal(err)
		}
		var list lsp.CompletionList
		if err := json.Unmarshal(result, &list); err != nil {
			t.Fatal(err)
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Label < list.Items[j].Label })
		return list
	}
}

// testSession starts a server on dir, with the test provider schema
// installed, opens every .tf file of dir and returns a function that sends
// a request, where the "uri" values name files of dir, and returns its
// result or error.
func testSession(t *testing.T, dir string, initOptions string, clientCaps string) func(method, params string) (json.RawMessage, error) {
	t.Helper()
	workDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	dirURI := uri.FromPath(workDir)

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
		StateStore: ss,
		TofuCalls: &exec.TofuMockCalls{
			PerWorkDir: map[string][]*mock.Call{
				workDir: {
					{
						Method:          "Version",
						Repeatability:   1,
						Arguments:       []interface{}{mock.AnythingOfType("")},
						ReturnArguments: []interface{}{version.Must(version.NewVersion("1.11.0")), nil, nil},
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
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)
	t.Cleanup(stop)

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	"capabilities": %s,
	"initializationOptions": %s,
	"rootUri": %q,
	"processId": 12345
}`, clientCaps, initOptions, dirURI)})
	waitForWalkerPath(t, ss, wc, document.DirHandle{URI: dirURI})
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(workDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text, _ := json.Marshal(string(content))
		ls.Call(t, &langserver.CallRequest{
			Method: "textDocument/didOpen",
			ReqParams: fmt.Sprintf(`{
	"textDocument": {
		"version": 0,
		"languageId": "opentofu",
		"text": %s,
		"uri": "%s/%s"
	}
}`, text, dirURI, e.Name())})
	}
	waitForAllJobs(t, ss)

	return func(method, params string) (json.RawMessage, error) {
		// "uri": "main.tf" names a file of dir
		var p map[string]any
		if err := json.Unmarshal([]byte(params), &p); err != nil {
			t.Fatal(err)
		}
		if td, ok := p["textDocument"].(map[string]any); ok {
			td["uri"] = dirURI + "/" + td["uri"].(string)
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		rsp, err := ls.CallWithError(t, &langserver.CallRequest{Method: method, ReqParams: string(b)})
		if err != nil {
			return nil, err
		}
		return rsp.Result, nil
	}
}

func TestCompletion_addRequiredProviders_currentFile(t *testing.T) {
	complete := providerCompletionSession(t, "testdata/required-providers-completion/current-file", "{}", "{}")

	list := complete("main.tf", 2, 10)
	labels := make([]string, 0)
	for _, item := range list.Items {
		labels = append(labels, item.Label)
		wantEdits := []lsp.TextEdit{
			{
				Range: lsp.Range{},
				NewText: `terraform {
  required_providers {
    test = {
      source = "test/test"
    }
  }
}

`,
			},
		}
		if diff := cmp.Diff(wantEdits, item.AdditionalTextEdits); diff != "" {
			t.Fatalf("%s: unexpected additional edits: %s", item.Label, diff)
		}
		if item.Command != nil {
			t.Fatalf("%s: unexpected command %#v", item.Label, item.Command)
		}
	}
	if diff := cmp.Diff([]string{"test_resource_1", "test_resource_2"}, labels); diff != "" {
		t.Fatalf("unexpected labels: %s", diff)
	}
}

func TestCompletion_addRequiredProviders_disabled(t *testing.T) {
	complete := providerCompletionSession(t, "testdata/required-providers-completion/current-file",
		`{"completion": {"addRequiredProviders": false}}`, "{}")

	list := complete("main.tf", 2, 10)
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list.Items))
	}
	for _, item := range list.Items {
		if len(item.AdditionalTextEdits) > 0 || item.Command != nil {
			t.Fatalf("%s: expected no declaration with the setting off: %#v", item.Label, item)
		}
	}
}

func TestCompletion_addRequiredProviders_otherFile(t *testing.T) {
	dir, err := filepath.Abs("testdata/required-providers-completion/other-file")
	if err != nil {
		t.Fatal(err)
	}
	complete := providerCompletionSession(t, dir, "{}", `{"workspace": {"applyEdit": true}}`)

	list := complete("main.tf", 2, 10)
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list.Items))
	}
	for _, item := range list.Items {
		if len(item.AdditionalTextEdits) > 0 {
			t.Fatalf("%s: the declaration belongs in versions.tf, got edits %#v", item.Label, item.AdditionalTextEdits)
		}
		args := make([]string, 0)
		for _, raw := range item.Command.Arguments {
			var arg string
			if err := json.Unmarshal(raw, &arg); err != nil {
				t.Fatal(err)
			}
			args = append(args, arg)
		}
		want := &lsp.Command{
			Title:   "Declare provider test in required_providers",
			Command: "tofu-ls.module.addRequiredProvider",
		}
		got := *item.Command
		got.Arguments = nil
		if diff := cmp.Diff(want, &got); diff != "" {
			t.Fatalf("%s: unexpected command: %s", item.Label, diff)
		}
		wantArgs := []string{
			"uri=" + uri.FromPath(filepath.Join(dir, "main.tf")),
			"name=test",
			"source=test/test",
		}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Fatalf("%s: unexpected arguments: %s", item.Label, diff)
		}
	}

	// without workspace/applyEdit support, the declaration is left out
	complete = providerCompletionSession(t, dir, "{}", "{}")
	for _, item := range complete("main.tf", 2, 10).Items {
		if item.Command != nil {
			t.Fatalf("%s: unexpected command without applyEdit support", item.Label)
		}
	}
}

func TestCompletion_requiredProvidersLocalNames(t *testing.T) {
	complete := providerCompletionSession(t, "testdata/required-providers-completion/local-names", "{}", `{
		"textDocument": {"completion": {"completionItem": {"snippetSupport": true}}}
	}`)

	list := complete("main.tf", 2, 0)
	got := make([]string, 0)
	for _, item := range list.Items {
		got = append(got, fmt.Sprintf("%s|%s|%s", item.Label, item.Detail, item.TextEdit.NewText))
	}
	want := []string{
		// the schema's placeholder
		"name|object or string|name = {\n  ${1}\n}",
		"test|test/test|test = {\n\tsource  = \"test/test\"\n\tversion = \"${1}\"\n}",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected items: %s", diff)
	}

	// with a prefix typed, only the matching names
	list = complete("main.tf", 3, 6)
	got = got[:0]
	for _, item := range list.Items {
		got = append(got, fmt.Sprintf("%s %d-%d", item.Label, item.TextEdit.Range.Start.Character, item.TextEdit.Range.End.Character))
	}
	if diff := cmp.Diff([]string{"test 4-6"}, got); diff != "" {
		t.Fatalf("unexpected items with a prefix: %s", diff)
	}
}

func TestAddRequiredProviderEdit(t *testing.T) {
	parse := func(src string) *hcl.File {
		f, _ := hclsyntax.ParseConfig([]byte(src), "x.tf", hcl.InitialPos)
		return f
	}
	dir := t.TempDir()
	files := map[string]*hcl.File{
		"main.tf":     parse("resource \"aws_instance\" \"a\" {}\n"),
		"versions.tf": parse("terraform {\n  required_providers {\n  }\n}\n"),
	}

	params, ok := addRequiredProviderEdit(files, dir, "main.tf", "aws", "hashicorp/aws")
	if !ok {
		t.Fatal("expected an edit")
	}
	want := lsp.ApplyWorkspaceEditParams{
		Label: "Declare provider aws",
		Edit: lsp.WorkspaceEdit{
			Changes: map[lsp.DocumentURI][]lsp.TextEdit{
				lsp.DocumentURI(uri.FromPath(filepath.Join(dir, "versions.tf"))): {
					{
						Range: lsp.Range{
							Start: lsp.Position{Line: 2, Character: 0},
							End:   lsp.Position{Line: 2, Character: 0},
						},
						NewText: "    aws = {\n      source = \"hashicorp/aws\"\n    }\n",
					},
				},
			},
		},
	}
	if diff := cmp.Diff(want, params); diff != "" {
		t.Fatalf("unexpected edit: %s", diff)
	}

	// declared meanwhile: nothing to do
	files["versions.tf"] = parse("terraform {\n  required_providers {\n    aws = { source = \"hashicorp/aws\" }\n  }\n}\n")
	if _, ok := addRequiredProviderEdit(files, dir, "main.tf", "aws", "hashicorp/aws"); ok {
		t.Fatal("expected no edit for a declared provider")
	}
}
