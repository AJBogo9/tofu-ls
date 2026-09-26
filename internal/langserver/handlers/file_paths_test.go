// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func TestFilePaths_documentLinks(t *testing.T) {
	dir, err := filepath.Abs("testdata/file-paths")
	if err != nil {
		t.Fatal(err)
	}
	request := testSession(t, dir, "{}", `{"textDocument": {"documentLink": {"tooltipSupport": true}}}`)

	var links []lsp.DocumentLink
	result, err := request("textDocument/documentLink", `{"textDocument": {"uri": "main.tf"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result, &links); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0)
	for _, l := range links {
		rel, _ := filepath.Rel(dir, uri.MustPathFromURI(l.Target))
		got = append(got, fmt.Sprintf("%d:%d-%d %s %s", l.Range.Start.Line, l.Range.Start.Character, l.Range.End.Character,
			filepath.ToSlash(rel), l.Tooltip))
	}
	want := []string{
		"1:20-52 templates/a.tftpl Open templates/a.tftpl",
		"2:12-27 files/data.json Open files/data.json",
		// through the local, with the static evaluator
		"6:11-18 files/data.json Open files/data.json",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected links: %s", diff)
	}
}

func TestFilePaths_definition(t *testing.T) {
	dir, err := filepath.Abs("testdata/file-paths")
	if err != nil {
		t.Fatal(err)
	}
	request := testSession(t, dir, "{}", "{}")

	testCases := []struct {
		name      string
		line, col int
		want      []string
	}{
		{"template path", 1, 40, []string{"templates/a.tftpl 0:0"}},
		{"path relative to the root module", 2, 15, []string{"files/data.json 0:0"}},
		// no target, or the error of a position without a reference
		{"missing file", 3, 40, []string{}},
		{"directory", 5, 18, []string{}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := request("textDocument/definition", fmt.Sprintf(`{
	"textDocument": {"uri": "main.tf"},
	"position": {"line": %d, "character": %d}
}`, tc.line, tc.col))
			if err != nil && len(tc.want) > 0 {
				t.Fatal(err)
			}
			var locations []lsp.Location
			if err == nil && string(result) != "null" {
				if err := json.Unmarshal(result, &locations); err != nil {
					t.Fatal(err)
				}
			}
			got := make([]string, 0)
			for _, l := range locations {
				rel, _ := filepath.Rel(dir, uri.MustPathFromURI(string(l.URI)))
				got = append(got, fmt.Sprintf("%s %d:%d", filepath.ToSlash(rel), l.Range.Start.Line, l.Range.Start.Character))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected definition: %s", diff)
			}
		})
	}
}

func TestFilePaths_completion(t *testing.T) {
	dir, err := filepath.Abs("testdata/file-paths")
	if err != nil {
		t.Fatal(err)
	}
	request := testSession(t, dir, "{}", "{}")

	testCases := []struct {
		name      string
		line, col int
		want      []string
	}{
		{
			"templatefile lists templates first",
			4, 45,
			[]string{
				"a.tftpl 17 a.tftpl 4:45-45",
				"b.txt 17 b.txt 4:45-45",
			},
		},
		{
			"file lists files and directories",
			2, 12,
			[]string{
				"main.tf 17 main.tf 2:12-17",
				"files 19 files/ 2:12-17",
				"templates 19 templates/ 2:12-17",
			},
		},
		{
			"typed prefix",
			2, 14,
			[]string{
				"files 19 files/ 2:12-17",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := request("textDocument/completion", fmt.Sprintf(`{
	"textDocument": {"uri": "main.tf"},
	"position": {"line": %d, "character": %d}
}`, tc.line, tc.col))
			if err != nil {
				t.Fatal(err)
			}
			var list lsp.CompletionList
			if err := json.Unmarshal(result, &list); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for _, item := range list.Items {
				got = append(got, fmt.Sprintf("%s %d %s %d:%d-%d", item.Label, item.Kind, item.TextEdit.NewText,
					item.TextEdit.Range.Start.Line, item.TextEdit.Range.Start.Character, item.TextEdit.Range.End.Character))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected items: %s", diff)
			}
		})
	}
}

// TestFilePaths_completionInCalledModule checks that a bare relative path
// in a module that other modules call is completed from the directory
// OpenTofu resolves it from, the root module's, and not from the module's
// own directory: from the only root that calls it, or not at all when
// several do. Paths from path.module stay relative to the module.
func TestFilePaths_completionInCalledModule(t *testing.T) {
	testCases := []struct {
		name      string
		tree      string
		line, col int
		want      []string
	}{
		{"one root", "one-root", 1, 12, []string{
			"main.tf 17 main.tf 1:12-12",
			"root.txt 17 root.txt 1:12-12",
			"modules 19 modules/ 1:12-12",
		}},
		{"one root, path.module", "one-root", 2, 27, []string{
			"app.txt 17 app.txt 2:27-27",
			"main.tf 17 main.tf 2:27-27",
		}},
		{"two roots", "two-roots", 1, 12, []string{}},
		{"two roots, path.module", "two-roots", 2, 27, []string{
			"app.txt 17 app.txt 2:27-27",
			"main.tf 17 main.tf 2:27-27",
		}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			request := testSession(t, filepath.Join("testdata", "file-paths-called", tc.tree, "modules", "app"), "{}", "{}")
			result, err := request("textDocument/completion", fmt.Sprintf(`{
	"textDocument": {"uri": "main.tf"},
	"position": {"line": %d, "character": %d}
}`, tc.line, tc.col))
			if err != nil {
				t.Fatal(err)
			}
			var list lsp.CompletionList
			if err := json.Unmarshal(result, &list); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for _, item := range list.Items {
				got = append(got, fmt.Sprintf("%s %d %s %d:%d-%d", item.Label, item.Kind, item.TextEdit.NewText,
					item.TextEdit.Range.Start.Line, item.TextEdit.Range.Start.Character, item.TextEdit.Range.End.Character))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected items: %s", diff)
			}
			if tc.name == "one root" && list.Items[1].Detail != "root.txt (in the root module ../..)" {
				t.Fatalf("the detail does not name the root: %q", list.Items[1].Detail)
			}
		})
	}
}
