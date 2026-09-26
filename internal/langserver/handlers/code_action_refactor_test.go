// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

// refactorClientCaps are the capabilities of VS Code with the
// vscode-opentofu client: code actions resolved later (their edits),
// disabled actions, file creation and the client's rename command.
const refactorClientCaps = `{
	"workspace": {"workspaceEdit": {"documentChanges": true, "resourceOperations": ["create", "rename", "delete"]}},
	"textDocument": {"codeAction": {"disabledSupport": true, "isPreferredSupport": true, "dataSupport": true,
		"resolveSupport": {"properties": ["edit"]}}},
	"experimental": {"refreshModuleCallsCommandId": "client.refreshModuleCalls", "renameCommandId": "client.rename"}
}`

// rawCodeActions requests code actions and returns them as JSON objects,
// to see which fields they have.
func (c *langserverMockCaller) rawCodeActions(t *testing.T, dir document.DirHandle, file string, rng lsp.Range, only []string, trigger int) []map[string]json.RawMessage {
	t.Helper()
	ctx := map[string]interface{}{"diagnostics": []lsp.Diagnostic{}, "triggerKind": trigger}
	if only != nil {
		ctx["only"] = only
	}
	params, err := json.Marshal(map[string]interface{}{
		"textDocument": map[string]string{"uri": dir.URI + "/" + file},
		"range":        rng,
		"context":      ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.call(&langserver.CallRequest{Method: "textDocument/codeAction", ReqParams: string(params)})
	if err != nil {
		t.Fatalf("code action request failed: %s", err)
	}
	var actions []map[string]json.RawMessage
	if err := json.Unmarshal(result, &actions); err != nil {
		t.Fatalf("%s: %s", result, err)
	}
	return actions
}

func rawTitled(t *testing.T, actions []map[string]json.RawMessage, title string) map[string]json.RawMessage {
	t.Helper()
	titles := make([]string, 0, len(actions))
	for _, a := range actions {
		var got string
		_ = json.Unmarshal(a["title"], &got)
		if got == title {
			return a
		}
		titles = append(titles, got)
	}
	t.Fatalf("no action %q among %q", title, titles)
	return nil
}

// resolve sends an action back to be resolved.
func (c *langserverMockCaller) resolve(t *testing.T, action interface{}) (lsp.CodeAction, error) {
	t.Helper()
	params, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.callWithError(&langserver.CallRequest{Method: "codeAction/resolve", ReqParams: string(params)})
	if err != nil {
		return lsp.CodeAction{}, err
	}
	var resolved lsp.CodeAction
	if err := json.Unmarshal(result, &resolved); err != nil {
		t.Fatalf("%s: %s", result, err)
	}
	return resolved, nil
}

func TestCodeAction_refactorings(t *testing.T) {
	files := map[string]string{
		"main.tf": `locals {
  zone = "a"
}

resource "terraform_data" "web" {
  input = "${local.zone}-web"
}
`,
	}
	ls, dir, stop := startCodeActionServer(t, files, refactorClientCaps, "main.tf")
	defer stop()
	sel := rangeOfText(t, files, "main.tf", `"${local.zone}-web"`, 1)

	// a cursor move asks for nothing
	cursor := lsp.Range{Start: sel.Start, End: sel.Start}
	if actions := ls.rawCodeActions(t, dir, "main.tf", cursor, nil, 2); len(actions) != 0 {
		t.Fatalf("expected no actions for a cursor move, got %v", actions)
	}

	// a selection in the lightbulb: an offer without an edit, which
	// would tell VS Code that there is nothing to resolve
	offer := rawTitled(t, ls.rawCodeActions(t, dir, "main.tf", sel, nil, 2), "Extract to local")
	if _, ok := offer["edit"]; ok {
		t.Fatalf("an offer has no edit, got %s", offer["edit"])
	}
	if string(offer["kind"]) != `"refactor.extract"` {
		t.Fatalf("unexpected kind %s", offer["kind"])
	}
	var data refactorData
	if err := json.Unmarshal(offer["data"], &data); err != nil || data.Refactoring != "extract-local" || data.Version != 0 {
		t.Fatalf("unexpected data %s (%v)", offer["data"], err)
	}

	resolved, err := ls.resolve(t, offer)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Title != "Extract to local" || resolved.Kind != "refactor.extract" || resolved.Data != nil {
		t.Fatalf("unexpected action %#v", resolved)
	}
	expectFiles(t, applyAction(t, dir, files, resolved), map[string]string{"main.tf": `locals {
  zone      = "a"
  web_input = "${local.zone}-web"
}

resource "terraform_data" "web" {
  input = local.web_input
}
`})
	cmd := resolved.Command
	if cmd == nil || cmd.Command != "client.rename" || len(cmd.Arguments) != 2 {
		t.Fatalf("expected a rename command, got %#v", cmd)
	}
	var target string
	var pos lsp.Position
	if json.Unmarshal(cmd.Arguments[0], &target) != nil || json.Unmarshal(cmd.Arguments[1], &pos) != nil ||
		target != dir.URI+"/main.tf" || pos != (lsp.Position{Line: 6, Character: 16}) {
		t.Fatalf("unexpected rename arguments %s", cmd.Arguments)
	}

	// asked for (Ctrl+.) at a local's name: inline it, but not delete it
	at := rangeOfText(t, files, "main.tf", "zone =", 1)
	at.End = at.Start
	actions := ls.rawCodeActions(t, dir, "main.tf", at, nil, 1)
	inline := rawTitled(t, actions, `Inline local "zone"`)
	del := rawTitled(t, actions, `Safe delete local "zone"`)
	if string(del["disabled"]) != `{"reason":"local.zone is used once: main.tf:6"}` {
		t.Fatalf("unexpected safe delete %v", del)
	}
	resolved, err = ls.resolve(t, inline)
	if err != nil {
		t.Fatal(err)
	}
	expectFiles(t, applyAction(t, dir, files, resolved), map[string]string{"main.tf": `resource "terraform_data" "web" {
  input = "a-web"
}
`})
	if resolved.Command != nil {
		t.Errorf("an inline has no command, got %#v", resolved.Command)
	}

	// only the kind asked for
	for _, a := range ls.rawCodeActions(t, dir, "main.tf", at, []string{"refactor.inline"}, 1) {
		if string(a["kind"]) != `"refactor.inline"` {
			t.Errorf("unexpected kind %s", a["kind"])
		}
	}

	// an action which is not a refactoring comes back as it is
	plain := map[string]interface{}{"title": "Run tofu init", "command": map[string]interface{}{"title": "Run tofu init", "command": "tofu.initCurrent"}}
	resolved, err = ls.resolve(t, plain)
	if err != nil || resolved.Title != "Run tofu init" || resolved.Command == nil || len(resolved.Edit.Changes) != 0 {
		t.Fatalf("unexpected %#v (%v)", resolved, err)
	}

	// an offer made on another version of the document is not resolved
	ls.call(&langserver.CallRequest{
		Method: "textDocument/didChange",
		ReqParams: fmt.Sprintf(`{
    "textDocument": {"version": 1, "uri": "%s/main.tf"},
    "contentChanges": [{"text": %q}]
}`, dir.URI, files["main.tf"]+"\n")})
	if _, err := ls.resolve(t, offer); err == nil || !strings.Contains(err.Error(), "main.tf changed after the refactoring was offered") {
		t.Fatalf("expected an error for a stale offer, got %v", err)
	}
}

func TestCodeAction_refactoringRefusedWhenResolved(t *testing.T) {
	files := map[string]string{
		"main.tf": "module \"child\" {\n  source = \"./child\"\n}\n",
		"child/main.tf": `resource "terraform_data" "c" {
  input = "x"
}
`,
	}
	ls, dir, stop := startCodeActionServer(t, files, refactorClientCaps, "child/main.tf")
	defer stop()

	rng := rangeOfText(t, files, "child/main.tf", `"x"`, 1)
	// the root module is only discovered: nothing knows yet that it
	// calls the child
	offer := rawTitled(t, ls.rawCodeActions(t, dir, "child/main.tf", rng, []string{"refactor"}, 1), "Introduce variable set in terraform.tfvars")
	if _, ok := offer["disabled"]; ok {
		t.Fatalf("expected an enabled offer, got %v", offer)
	}
	// resolving it looks for every caller, and refuses: the user is told
	// why (window/showMessage), and nothing is applied
	resolved, err := ls.resolve(t, offer)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Edit.Changes) != 0 || len(resolved.Edit.DocumentChanges) != 0 || resolved.Command != nil || resolved.Data != nil {
		t.Fatalf("expected no edit, got %#v", resolved)
	}
	// now the callers are indexed, and the offer says so
	actions := ls.rawCodeActions(t, dir, "child/main.tf", rng, []string{"refactor"}, 1)
	offer = rawTitled(t, actions, "Introduce variable set in terraform.tfvars")
	if string(offer["disabled"]) != `{"reason":"module.child (in ..) calls this module, and terraform.tfvars sets only the variables of a root module"}` {
		t.Fatalf("unexpected offer %v", offer)
	}

	// the variable with a default is fine in a child module
	resolved, err = ls.resolve(t, rawTitled(t, actions, "Introduce variable"))
	if err != nil {
		t.Fatal(err)
	}
	expectFiles(t, applyAction(t, dir, files, resolved), map[string]string{
		"child/main.tf":      "resource \"terraform_data\" \"c\" {\n  input = var.c_input\n}\n",
		"child/variables.tf": "variable \"c_input\" {\n  type    = string\n  default = \"x\"\n}\n",
	})
}

func TestCodeAction_refactoringsWithoutResolve(t *testing.T) {
	files := map[string]string{
		"main.tf": `variable "names" {
  type    = list(string)
  default = ["a", "b"]
}

resource "terraform_data" "t" {
  count = length(var.names)
  input = var.names[count.index]
}

output "first" {
  value = terraform_data.t[0].input
}
`,
	}
	// a client which cannot resolve edits later gets them at once, and no
	// command it has not announced
	ls, dir, stop := startCodeActionServer(t, files, `{"textDocument": {"codeAction": {"disabledSupport": true}}}`, "main.tf")
	defer stop()

	at := rangeOfText(t, files, "main.tf", "count", 1)
	at.End = at.Start
	actions := ls.codeActions(t, dir, "main.tf", at, []string{"refactor.rewrite"})
	// the list's value comes from the static evaluator: the default
	a := actionTitled(t, actions, "Convert count to for_each with 2 moved blocks")
	if a.Data != nil || a.Command != nil {
		t.Fatalf("expected a complete action, got %#v", a)
	}
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{"main.tf": `variable "names" {
  type    = list(string)
  default = ["a", "b"]
}

resource "terraform_data" "t" {
  for_each = toset(var.names)
  input    = each.value
}

output "first" {
  value = terraform_data.t["a"].input
}

moved {
  from = terraform_data.t[0]
  to   = terraform_data.t["a"]
}

moved {
  from = terraform_data.t[1]
  to   = terraform_data.t["b"]
}
`})

	// without its keys, the conversion says why it is refused
	files["main.tf"] = strings.Replace(files["main.tf"], "  default = [\"a\", \"b\"]\n", "", 1)
	ls2, dir2, stop2 := startCodeActionServer(t, files, `{"textDocument": {"codeAction": {"disabledSupport": true}}}`, "main.tf")
	defer stop2()
	at = rangeOfText(t, files, "main.tf", "count", 1)
	at.End = at.Start
	a = actionTitled(t, ls2.codeActions(t, dir2, "main.tf", at, []string{"refactor"}), "Convert count to for_each")
	if a.Disabled == nil || !strings.HasPrefix(a.Disabled.Reason, "the value of var.names is not known statically") {
		t.Fatalf("expected a disabled action, got %#v", a)
	}
}
