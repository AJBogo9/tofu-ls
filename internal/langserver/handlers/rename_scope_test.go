// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/creachadair/jrpc2"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

// at is the position of the nth occurrence of needle on the 0-based line
// of a file, offset characters into it (ASCII sources only).
type at struct {
	file   string
	line   int
	needle string
	nth    int
	offset int
}

func (a at) find(t *testing.T, files map[string]string) (int, int) {
	t.Helper()
	lines := strings.Split(files[a.file], "\n")
	if a.line >= len(lines) {
		t.Fatalf("%s has no line %d", a.file, a.line)
	}
	idx := -1
	for i := 0; i < a.nth; i++ {
		next := strings.Index(lines[a.line][idx+1:], a.needle)
		if next < 0 {
			t.Fatalf("%q (occurrence %d) not on %s:%d: %q", a.needle, a.nth, a.file, a.line, lines[a.line])
		}
		idx += next + 1
	}
	return a.line, idx + a.offset
}

func (a at) position(t *testing.T, files map[string]string) string {
	line, char := a.find(t, files)
	return fmt.Sprintf(`{"line": %d, "character": %d}`, line, char)
}

// renameEdit replaces the needle found by at (without its offset).
type renameEdit struct {
	at
	newText string
}

// eofEdit inserts text at the end of a file.
type eofEdit struct {
	file string
	text string
}

// workspaceEditResponse is the expected response of a rename, with the
// changes marshalled as the server marshals them.
func workspaceEditResponse(t *testing.T, tmpDir document.DirHandle, files map[string]string, id int, edits []renameEdit, eof ...eofEdit) string {
	t.Helper()
	changes := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, e := range edits {
		line, char := e.find(t, files)
		char -= e.offset
		uri := lsp.DocumentURI(tmpDir.URI + "/" + e.file)
		changes[uri] = append(changes[uri], lsp.TextEdit{
			Range: lsp.Range{
				Start: lsp.Position{Line: uint32(line), Character: uint32(char)},
				End:   lsp.Position{Line: uint32(line), Character: uint32(char + len(e.needle))},
			},
			NewText: e.newText,
		})
	}
	for uri := range changes {
		sort.Slice(changes[uri], func(i, j int) bool {
			a, b := changes[uri][i].Range.Start, changes[uri][j].Range.Start
			return a.Line < b.Line || (a.Line == b.Line && a.Character < b.Character)
		})
	}
	for _, e := range eof {
		lines := strings.Split(files[e.file], "\n")
		last := uint32(len(lines) - 1)
		end := lsp.Position{Line: last, Character: uint32(len(lines[last]))}
		uri := lsp.DocumentURI(tmpDir.URI + "/" + e.file)
		changes[uri] = append(changes[uri], lsp.TextEdit{Range: lsp.Range{Start: end, End: end}, NewText: e.text})
	}
	result, err := json.Marshal(lsp.WorkspaceEdit{Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc": "2.0", "id": %d, "result": %s}`, id, result)
}

func renameRequest(tmpDir document.DirHandle, files map[string]string, t *testing.T, a at, newName string) *langserver.CallRequest {
	return &langserver.CallRequest{
		Method: "textDocument/rename",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {"uri": "%s/%s"},
			"position": %s,
			"newName": %q
		}`, tmpDir.URI, a.file, a.position(t, files), newName)}
}

func TestRename_overrideFileSortsFirst(t *testing.T) {
	files := map[string]string{
		"main.tf": `resource "terraform_data" "web" {
  input = var.region
}

output "id" {
  value = terraform_data.web.id
}
`,
		"override.tf": `variable "region" {
  default = "b"
}

resource "terraform_data" "web" {
  triggers_replace = [var.region]
}
`,
		"variables.tf": `variable "region" {
  type    = string
  default = "a"
}
`,
	}
	ls, tmpDir, stop := startNavigationServerOpening(t, files, "{}", "override.tf")
	defer stop()

	regionEdits := []renameEdit{
		{at{"main.tf", 1, "region", 1, 0}, "location"},
		{at{"override.tf", 0, "region", 1, 0}, "location"},
		{at{"override.tf", 5, "region", 1, 0}, "location"},
		{at{"variables.tf", 0, "region", 1, 0}, "location"},
	}
	// from the use: the primary declaration in variables.tf too, although
	// override.tf sorts first
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"main.tf", 1, "region", 1, 2}, "location"),
		workspaceEditResponse(t, tmpDir, files, 4, regionEdits))
	// from the override's label: the primary declaration too
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"override.tf", 0, "region", 1, 2}, "location"),
		workspaceEditResponse(t, tmpDir, files, 5, regionEdits))

	// a resource: the moved block goes to the primary file, since
	// OpenTofu rejects moved blocks in override files
	webEdits := []renameEdit{
		{at{"main.tf", 0, "web", 1, 0}, "site"},
		{at{"main.tf", 5, "web", 1, 0}, "site"},
		{at{"override.tf", 4, "web", 1, 0}, "site"},
	}
	moved := eofEdit{"main.tf", "\nmoved {\n  from = terraform_data.web\n  to   = terraform_data.site\n}\n"}
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"main.tf", 5, "web", 1, 1}, "site"),
		workspaceEditResponse(t, tmpDir, files, 6, webEdits, moved))
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"override.tf", 4, "web", 1, 1}, "site"),
		workspaceEditResponse(t, tmpDir, files, 7, webEdits, moved))
}

func TestRename_outputRefusesModuleObjectUses(t *testing.T) {
	child := "output \"name\" {\n  value = \"x\"\n}\n"
	call := `module "kids" {
  for_each = toset(["a", "b"])
  source   = "./app"
}
`
	testCases := []struct {
		name    string
		use     string
		wantErr string
	}{
		{
			"values() iterated by a for expression",
			"output \"names\" {\n  value = [for m in values(module.kids) : m.name]\n}\n",
			"module.kids is iterated by a for expression (main.tf:6), whose uses of name cannot be renamed safely; rename them by hand",
		},
		{
			"the whole object in an output",
			"output \"all\" {\n  value = module.kids\n}\n",
			"module.kids is used as a whole object (main.tf:6), whose uses of name cannot be renamed safely; rename them by hand",
		},
		{
			"one instance in a local",
			"locals {\n  a = module.kids[\"a\"]\n}\n",
			"module.kids is used as a whole object (main.tf:6), whose uses of name cannot be renamed safely; rename them by hand",
		},
		{
			"iterated by for_each",
			"resource \"terraform_data\" \"per_kid\" {\n  for_each = module.kids\n}\n",
			"module.kids is iterated by for_each (main.tf:6), whose uses of name cannot be renamed safely; rename them by hand",
		},
		{
			"all instances by a splat",
			"locals {\n  all = module.kids[*]\n}\n",
			"module.kids is used as a whole object (main.tf:6), whose uses of name cannot be renamed safely; rename them by hand",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"main.tf": call + tc.use, "app/outputs.tf": child}
			ls, tmpDir, stop := startNavigationServerOpening(t, files, "{}", "app/outputs.tf")
			defer stop()

			ls.CallAndExpectError(t, renameRequest(tmpDir, files, t, at{"app/outputs.tf", 0, "name", 1, 1}, "title"),
				&jrpc2.Error{Code: jrpc2.Code(-32098), Message: tc.wantErr})
		})
	}
}

func TestRename_outputThroughModuleObjectUsesThatKeepIt(t *testing.T) {
	files := map[string]string{
		"main.tf": `module "kids" {
  for_each = toset(["a", "b"])
  source   = "./app"
}
resource "terraform_data" "after" {
  count      = length(module.kids)
  input      = keys(module.kids)
  depends_on = [module.kids]
}
locals {
  names = module.kids[*].name
  k     = "a"
  one   = module.kids[local.k].name
}
`,
		"app/outputs.tf": "output \"name\" {\n  value = \"x\"\n}\n",
	}
	ls, tmpDir, stop := startNavigationServerOpening(t, files, "{}", "app/outputs.tf")
	defer stop()

	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"app/outputs.tf", 0, "name", 1, 1}, "title"),
		workspaceEditResponse(t, tmpDir, files, 4, []renameEdit{
			{at{"app/outputs.tf", 0, "name", 1, 0}, "title"},
			{at{"main.tf", 10, "name", 2, 0}, "title"},
			{at{"main.tf", 12, "name", 1, 0}, "title"},
		}))
}

func TestRename_indexedChildOutputUse(t *testing.T) {
	files := map[string]string{
		"main.tf": `module "kids" {
  for_each = toset(["a", "b"])
  source   = "./app"
}
module "child" {
  count  = 1
  source = "./app"
}
locals {
  k     = "a"
  a     = module.kids["a"].name
  all   = module.child[*].name
  first = module.child[0].name
  one   = module.kids[local.k].name
}
`,
		"app/outputs.tf": "output \"name\" {\n  value = \"x\"\n}\n",
	}
	ls, tmpDir, stop := startNavigationServer(t, files)
	defer stop()

	edits := []renameEdit{
		{at{"app/outputs.tf", 0, "name", 1, 0}, "title"},
		{at{"main.tf", 10, "name", 1, 0}, "title"},
		{at{"main.tf", 11, "name", 1, 0}, "title"},
		{at{"main.tf", 12, "name", 1, 0}, "title"},
		{at{"main.tf", 13, "name", 1, 0}, "title"},
	}
	id := 3
	for line := 10; line <= 13; line++ {
		cursor := at{"main.tf", line, "name", 1, 2}
		ls.CallAndExpectResponse(t, &langserver.CallRequest{
			Method: "textDocument/prepareRename",
			ReqParams: fmt.Sprintf(`{
				"textDocument": {"uri": "%s/main.tf"},
				"position": %s
			}`, tmpDir.URI, cursor.position(t, files))}, fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": %d,
				"result": {
					"range": {"start": {"line": %d, "character": %d}, "end": {"line": %d, "character": %d}},
					"placeholder": "name"
				}
			}`, id, line, cursor.offsetFree(t, files), line, cursor.offsetFree(t, files)+4))
		id++
		ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, cursor, "title"),
			workspaceEditResponse(t, tmpDir, files, id, edits))
		id++
	}

	// on the key inside the index: the local, not the output
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"main.tf", 13, "local.k", 1, 6}, "key"),
		workspaceEditResponse(t, tmpDir, files, id, []renameEdit{
			{at{"main.tf", 9, "k", 1, 0}, "key"},
			{at{"main.tf", 13, "k", 2, 0}, "key"},
		}))
}

// offsetFree is the character of the needle itself.
func (a at) offsetFree(t *testing.T, files map[string]string) int {
	_, char := a.find(t, files)
	return char - a.offset
}

const runScopeRootTf = `variable "stage" {
  type    = string
  default = "dev"
}

output "root_stage" {
  value = var.stage
}
`

const runScopeChildTf = `variable "stage" {
  type = string
}

output "child_stage" {
  value = "child-${var.stage}"
}
`

const runScopeTest = `run "root" {
  command = plan

  variables {
    stage = "prod"
  }

  assert {
    condition     = output.root_stage == "prod" && var.stage == "prod"
    error_message = "root"
  }
}

run "child" {
  command = apply

  module {
    source = "./modules/child"
  }

  variables {
    stage = "qa"
  }

  assert {
    condition     = output.child_stage == "child-qa" && var.stage == "qa"
    error_message = "child"
  }
}

run "later" {
  command = plan

  variables {
    stage = run.child.child_stage
  }

  assert {
    condition     = output.root_stage == run.child.child_stage
    error_message = "later"
  }
}
`

func TestRename_testFilesFollowTheRunModule(t *testing.T) {
	files := map[string]string{
		"main.tf":               runScopeRootTf,
		"modules/child/main.tf": runScopeChildTf,
		"tests/main.tftest.hcl": runScopeTest,
	}
	ls, tmpDir, stop := startNavigationServerOpening(t, files, "{}", "modules/child/main.tf")
	defer stop()

	// the root variable: runs "root" and "later" run the root module,
	// run "child" sets and checks the child's own stage
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"main.tf", 0, "stage", 1, 1}, "tier"),
		workspaceEditResponse(t, tmpDir, files, 4, []renameEdit{
			{at{"main.tf", 0, "stage", 1, 0}, "tier"},
			{at{"main.tf", 6, "stage", 1, 0}, "tier"},
			{at{"tests/main.tftest.hcl", 4, "stage", 1, 0}, "tier"},
			{at{"tests/main.tftest.hcl", 8, "stage", 2, 0}, "tier"},
			{at{"tests/main.tftest.hcl", 34, "stage", 1, 0}, "tier"},
		}))

	// the child's variable, from the root's tests that run the child
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"modules/child/main.tf", 0, "stage", 1, 1}, "tier"),
		workspaceEditResponse(t, tmpDir, files, 5, []renameEdit{
			{at{"modules/child/main.tf", 0, "stage", 1, 0}, "tier"},
			{at{"modules/child/main.tf", 5, "stage", 1, 0}, "tier"},
			{at{"tests/main.tftest.hcl", 21, "stage", 1, 0}, "tier"},
			{at{"tests/main.tftest.hcl", 25, "stage", 2, 0}, "tier"},
		}))

	// the child's output: its assertion and run.child.<output>
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"modules/child/main.tf", 4, "child_stage", 1, 1}, "label"),
		workspaceEditResponse(t, tmpDir, files, 6, []renameEdit{
			{at{"modules/child/main.tf", 4, "child_stage", 1, 0}, "label"},
			{at{"tests/main.tftest.hcl", 25, "child_stage", 1, 0}, "label"},
			{at{"tests/main.tftest.hcl", 34, "child_stage", 1, 0}, "label"},
			{at{"tests/main.tftest.hcl", 38, "child_stage", 1, 0}, "label"},
		}))

	// the root's output: its assertions, not run.child.child_stage
	ls.CallAndExpectResponse(t, renameRequest(tmpDir, files, t, at{"main.tf", 5, "root_stage", 1, 1}, "stage_out"),
		workspaceEditResponse(t, tmpDir, files, 7, []renameEdit{
			{at{"main.tf", 5, "root_stage", 1, 0}, "stage_out"},
			{at{"tests/main.tftest.hcl", 8, "root_stage", 1, 0}, "stage_out"},
			{at{"tests/main.tftest.hcl", 38, "root_stage", 1, 0}, "stage_out"},
		}))
}

func TestRename_refusesFileVariablesSharedWithAnotherModule(t *testing.T) {
	files := map[string]string{
		"main.tf":               runScopeRootTf,
		"modules/child/main.tf": runScopeChildTf,
		"tests/main.tftest.hcl": `variables {
  stage = "shared"
}

run "root" {
  command = plan
}

run "child" {
  command = plan

  module {
    source = "./modules/child"
  }
}
`,
	}
	ls, tmpDir, stop := startNavigationServer(t, files)
	defer stop()

	ls.CallAndExpectError(t, renameRequest(tmpDir, files, t, at{"main.tf", 0, "stage", 1, 1}, "tier"),
		&jrpc2.Error{Code: jrpc2.Code(-32098), Message: `tests/main.tftest.hcl sets stage for every run, and run "child", which runs modules/child, reads it too; rename it there by hand`})
}
