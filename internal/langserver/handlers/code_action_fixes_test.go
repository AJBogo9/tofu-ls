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

	"github.com/hashicorp/go-version"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/walker"
	"github.com/stretchr/testify/mock"
)

// codeActionClientCaps are the capabilities of VS Code which the code
// actions depend on: file creation in workspace edits, disabled actions,
// and the experimental capability of the vscode-opentofu client.
const codeActionClientCaps = `{
	"workspace": {"workspaceEdit": {"documentChanges": true, "resourceOperations": ["create", "rename", "delete"]}},
	"textDocument": {"codeAction": {"disabledSupport": true, "isPreferredSupport": true}},
	"experimental": {"refreshModuleCallsCommandId": "client.refreshModuleCalls"}
}`

// startCodeActionServer writes files into a temporary workspace, opens
// the named files (tfvars files as opentofu-vars) and waits until every
// module is indexed.
func startCodeActionServer(t *testing.T, files map[string]string, caps string, open ...string) (*langserverMockCaller, document.DirHandle, func()) {
	tmpDir := TempDir(t)
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
	    "capabilities": %s,
	    "rootUri": %q,
	    "processId": 12345
	}`, caps, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	for _, name := range open {
		languageID := "opentofu"
		if strings.HasSuffix(name, ".tfvars") {
			languageID = "opentofu-vars"
		}
		ls.Call(t, &langserver.CallRequest{
			Method: "textDocument/didOpen",
			ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": %q,
			"text": %q,
			"uri": "%s/%s"
		}
	}`, languageID, files[name], tmpDir.URI, name)})
	}
	waitForAllJobs(t, ss)

	call := func(cr *langserver.CallRequest) (json.RawMessage, error) {
		rsp := ls.Call(t, cr)
		if rsp.Error != nil {
			return nil, rsp.Error
		}
		return rsp.Result, nil
	}
	callWithError := func(cr *langserver.CallRequest) (json.RawMessage, error) {
		rsp, err := ls.CallWithError(t, cr)
		if err != nil {
			return nil, err
		}
		return rsp.Result, nil
	}
	return &langserverMockCaller{call: call, callWithError: callWithError}, tmpDir, stop
}

// langserverMockCaller sends requests to a server started for one test.
type langserverMockCaller struct {
	call func(cr *langserver.CallRequest) (json.RawMessage, error)
	// callWithError returns an error response rather than failing the test.
	callWithError func(cr *langserver.CallRequest) (json.RawMessage, error)
}

// codeActions requests the code actions of a range of a file.
func (c *langserverMockCaller) codeActions(t *testing.T, dir document.DirHandle, file string, rng lsp.Range, only []string, diags ...lsp.Diagnostic) []lsp.CodeAction {
	t.Helper()
	ctx := map[string]interface{}{"diagnostics": diags}
	if diags == nil {
		ctx["diagnostics"] = []lsp.Diagnostic{}
	}
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
	var actions []lsp.CodeAction
	if err := json.Unmarshal(result, &actions); err != nil {
		t.Fatalf("%s: %s", result, err)
	}
	return actions
}

// rangeOfText is the LSP range of the nth occurrence of needle in a file
// (ASCII sources only).
func rangeOfText(t *testing.T, files map[string]string, file, needle string, nth int) lsp.Range {
	t.Helper()
	src := files[file]
	idx := -1
	for i := 0; i < nth; i++ {
		next := strings.Index(src[idx+1:], needle)
		if next < 0 {
			t.Fatalf("%q (occurrence %d) not in %s", needle, nth, file)
		}
		idx += next + 1
	}
	return lsp.Range{Start: lspPosAt(src, idx), End: lspPosAt(src, idx+len(needle))}
}

func lspPosAt(src string, offset int) lsp.Position {
	line := strings.Count(src[:offset], "\n")
	col := offset - (strings.LastIndex(src[:offset], "\n") + 1)
	return lsp.Position{Line: uint32(line), Character: uint32(col)}
}

func offsetAt(t *testing.T, src string, pos lsp.Position) int {
	t.Helper()
	lines := strings.SplitAfter(src, "\n")
	if int(pos.Line) > len(lines) {
		t.Fatalf("position %v past the end", pos)
	}
	off := 0
	for i := 0; i < int(pos.Line); i++ {
		off += len(lines[i])
	}
	return off + int(pos.Character)
}

// applyAction applies an action's workspace edit to files (relative to
// dir) and returns the files it changes or creates.
func applyAction(t *testing.T, dir document.DirHandle, files map[string]string, a lsp.CodeAction) map[string]string {
	t.Helper()
	edits := map[string][]lsp.TextEdit{}
	for uri, e := range a.Edit.Changes {
		edits[string(uri)] = append(edits[string(uri)], e...)
	}
	created := map[string]bool{}
	for _, dc := range a.Edit.DocumentChanges {
		switch {
		case dc.CreateFile != nil:
			created[string(dc.CreateFile.URI)] = true
			if _, ok := edits[string(dc.CreateFile.URI)]; !ok {
				edits[string(dc.CreateFile.URI)] = nil
			}
		case dc.TextDocumentEdit != nil:
			uri := string(dc.TextDocumentEdit.TextDocument.URI)
			edits[uri] = append(edits[uri], dc.TextDocumentEdit.Edits...)
		default:
			t.Fatalf("unexpected document change %#v", dc)
		}
	}
	out := map[string]string{}
	for uri, list := range edits {
		rel := strings.TrimPrefix(uri, dir.URI+"/")
		src, ok := files[rel]
		if !ok && !created[uri] {
			t.Fatalf("edit of %s, which neither exists nor is created", rel)
		}
		sort.Slice(list, func(i, j int) bool {
			return offsetAt(t, src, list[i].Range.Start) > offsetAt(t, src, list[j].Range.Start)
		})
		for _, e := range list {
			start, end := offsetAt(t, src, e.Range.Start), offsetAt(t, src, e.Range.End)
			src = src[:start] + e.NewText + src[end:]
		}
		out[rel] = src
	}
	return out
}

func actionTitled(t *testing.T, actions []lsp.CodeAction, title string) lsp.CodeAction {
	t.Helper()
	for _, a := range actions {
		if a.Title == title {
			return a
		}
	}
	titles := make([]string, 0, len(actions))
	for _, a := range actions {
		titles = append(titles, a.Title)
	}
	t.Fatalf("no action %q among %q", title, titles)
	return lsp.CodeAction{}
}

func expectFiles(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("expected %d changed files, got %d: %v", len(want), len(got), got)
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("%s:\nexpected\n%s\ngot\n%s", name, content, got[name])
		}
	}
}

func diagnostic(rng lsp.Range, code string, data map[string]interface{}) lsp.Diagnostic {
	d := lsp.Diagnostic{Range: rng, Severity: lsp.SeverityHint, Source: "OpenTofu", Code: code, Message: code}
	if data != nil {
		d.Data = data
	}
	return d
}

func TestCodeAction_removeUnusedVariableEverywhere(t *testing.T) {
	files := map[string]string{
		"main.tf": `module "child" {
  source = "./child"
  name   = "a"
  unused = "b"
}
`,
		"child/variables.tf": `variable "name" {}

variable "unused" {}
`,
		"child/main.tf": `resource "terraform_data" "d" {
  input = var.name
}
`,
		"child/terraform.tfvars": "name   = \"x\"\nunused = \"y\"\n",
		"child/tests/child.tftest.hcl": `run "one" {
  variables {
    unused = "z"
  }
}
`,
	}
	ls, dir, stop := startCodeActionServer(t, files, codeActionClientCaps, "child/variables.tf")
	defer stop()

	label := rangeOfText(t, files, "child/variables.tf", `"unused"`, 1)
	// the lightbulb asks for no particular kind
	actions := ls.codeActions(t, dir, "child/variables.tf", label, nil,
		diagnostic(label, "unused-variable", map[string]interface{}{"name": "unused"}))
	a := actionTitled(t, actions, `Remove unused variable "unused" and where it is set (../main.tf, terraform.tfvars, tests/child.tftest.hcl)`)
	if a.Kind != "quickfix" || !a.IsPreferred || len(a.Diagnostics) != 1 {
		t.Errorf("expected a preferred quick fix for the diagnostic, got %#v", a)
	}
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{
		"main.tf": `module "child" {
  source = "./child"
  name   = "a"
}
`,
		"child/variables.tf":           "variable \"name\" {}\n",
		"child/terraform.tfvars":       "name = \"x\"\n",
		"child/tests/child.tftest.hcl": "run \"one\" {\n}\n",
	})
}

func TestCodeAction_quickFixes(t *testing.T) {
	files := map[string]string{
		"main.tf": `module "child" {
  source = "./child"
}

output "o" {}

locals {
  unused = 1
  path   = "${var.missing}"
}

resource "terraform_data" "d" {
  input = local.path
}
`,
		"child/main.tf": `variable "name" {
  type = string
}

output "name" {
  value = var.name
}
`,
		"terraform.tfvars": "extra = [1, 2]\n",
	}
	ls, dir, stop := startCodeActionServer(t, files, codeActionClientCaps, "main.tf", "terraform.tfvars")
	defer stop()

	testCases := []struct {
		name  string
		file  string
		diag  lsp.Diagnostic
		title string
		want  map[string]string
	}{
		{
			name:  "a required module input, typed by the child's variable",
			file:  "main.tf",
			diag:  diagnostic(rangeOfText(t, files, "main.tf", "{\n  source = \"./child\"\n}", 1), "missing-required-attribute", map[string]interface{}{"attributes": []string{"name"}}),
			title: `Add required argument "name"`,
			want: map[string]string{"main.tf": strings.Replace(files["main.tf"], `  source = "./child"
`, `  source = "./child"
  name   = ""
`, 1)},
		},
		{
			name:  "an output without a value",
			file:  "main.tf",
			diag:  diagnostic(rangeOfText(t, files, "main.tf", "{}", 1), "missing-required-attribute", nil),
			title: `Add required argument "value"`,
			want:  map[string]string{"main.tf": strings.Replace(files["main.tf"], `output "o" {}`, "output \"o\" {\n  value = null\n}", 1)},
		},
		{
			name:  "an unused local",
			file:  "main.tf",
			diag:  diagnostic(rangeOfText(t, files, "main.tf", "unused", 1), "unused-local", map[string]interface{}{"name": "unused"}),
			title: `Remove unused local "unused"`,
			want:  map[string]string{"main.tf": strings.Replace(files["main.tf"], "  unused = 1\n  path   =", "  path =", 1)},
		},
		{
			name:  "an interpolation-only template",
			file:  "main.tf",
			diag:  diagnostic(rangeOfText(t, files, "main.tf", `"${var.missing}"`, 1), "interpolation-only", nil),
			title: `Replace "${var.missing}" with var.missing`,
			want:  map[string]string{"main.tf": strings.Replace(files["main.tf"], `"${var.missing}"`, "var.missing", 1)},
		},
		{
			name:  "an undeclared variable, in a new file",
			file:  "main.tf",
			diag:  diagnostic(rangeOfText(t, files, "main.tf", "var.missing", 1), "unresolved-reference", map[string]interface{}{"address": "var.missing"}),
			title: `Declare variable "missing" in a new variables.tf`,
			want:  map[string]string{"variables.tf": "variable \"missing\" {}\n"},
		},
		{
			name:  "a variable set only in terraform.tfvars",
			file:  "terraform.tfvars",
			diag:  diagnostic(rangeOfText(t, files, "terraform.tfvars", "extra", 1), "tfvars-undeclared-variable", map[string]interface{}{"name": "extra", "valueType": "tuple([number, number])"}),
			title: `Declare variable "extra" in a new variables.tf`,
			want:  map[string]string{"variables.tf": "variable \"extra\" {\n  type = list(number)\n}\n"},
		},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			actions := ls.codeActions(t, dir, tc.file, tc.diag.Range, []string{"quickfix"}, tc.diag)
			a := actionTitled(t, actions, tc.title)
			if a.Kind != "quickfix" || len(a.Diagnostics) != 1 || a.Diagnostics[0].Code != tc.diag.Code {
				t.Errorf("expected a quick fix for its diagnostic, got %#v", a)
			}
			expectFiles(t, applyAction(t, dir, files, a), tc.want)
		})
	}

	// init runs through the client's command, for the module directory
	rng := rangeOfText(t, files, "main.tf", `"./child"`, 1)
	actions := ls.codeActions(t, dir, "main.tf", rng, nil,
		diagnostic(rng, "module-not-installed", map[string]interface{}{"module": "child", "dir": dir.Path()}))
	a := actionTitled(t, actions, "Run tofu init")
	if a.Command == nil || a.Command.Command != "tofu.initCurrent" || len(a.Command.Arguments) != 1 ||
		string(a.Command.Arguments[0]) != fmt.Sprintf("%q", dir.URI) || len(a.Edit.Changes)+len(a.Edit.DocumentChanges) != 0 {
		t.Errorf("unexpected init action %#v", a)
	}
}

func TestCodeAction_withoutClientSupport(t *testing.T) {
	files := map[string]string{
		"main.tf": "locals {\n  x = var.missing\n}\n\nmodule \"m\" {\n  source = \"./m\"\n}\n",
	}
	ls, dir, stop := startCodeActionServer(t, files, "{}", "main.tf")
	defer stop()

	// without file creation the variable goes into the document
	rng := rangeOfText(t, files, "main.tf", "var.missing", 1)
	actions := ls.codeActions(t, dir, "main.tf", rng, nil, diagnostic(rng, "unresolved-reference", nil))
	a := actionTitled(t, actions, `Declare variable "missing" in main.tf`)
	if len(a.Edit.DocumentChanges) != 0 {
		t.Errorf("expected plain changes, got %#v", a.Edit.DocumentChanges)
	}
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{"main.tf": files["main.tf"] + "\nvariable \"missing\" {}\n"})

	// a client which has not announced its init command gets no init action
	rng = rangeOfText(t, files, "main.tf", `"./m"`, 1)
	if actions := ls.codeActions(t, dir, "main.tf", rng, nil, diagnostic(rng, "module-not-installed", nil)); len(actions) != 0 {
		t.Errorf("expected no actions, got %#v", actions)
	}

	// nor a disabled rewrite it could not show
	files["main.tf"] = "locals {\n  x = element(var.l, 3)\n}\n"
	ls2, dir2, stop2 := startCodeActionServer(t, files, "{}", "main.tf")
	defer stop2()
	rng = rangeOfText(t, files, "main.tf", "element", 1)
	if actions := ls2.codeActions(t, dir2, "main.tf", rng, nil); len(actions) != 0 {
		t.Errorf("expected no actions, got %#v", actions)
	}
}

func TestCodeAction_intentions(t *testing.T) {
	files := map[string]string{
		"main.tf": `locals {
  zones = ["a", "b"]
  first = element(local.zones, 0)
  wraps = element(local.zones, 5)
  port  = lookup(var.ports, "web")
}

resource "random_pet" "p" {}
`,
	}
	ls, dir, stop := startCodeActionServer(t, files, codeActionClientCaps, "main.tf")
	defer stop()

	at := func(needle string) lsp.Range {
		r := rangeOfText(t, files, "main.tf", needle, 1)
		r.Start.Character++
		r.End = r.Start
		return r
	}

	// the lightbulb asks for everything at the cursor
	a := actionTitled(t, ls.codeActions(t, dir, "main.tf", at("element(local.zones, 0)"), nil), "Replace element() with local.zones[0]")
	if a.Kind != "refactor.rewrite" || a.Disabled != nil {
		t.Errorf("unexpected action %#v", a)
	}
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{"main.tf": strings.Replace(files["main.tf"], "element(local.zones, 0)", "local.zones[0]", 1)})

	a = actionTitled(t, ls.codeActions(t, dir, "main.tf", at("element(local.zones, 5)"), []string{"refactor"}), "Replace element() with an index")
	if a.Disabled == nil || a.Disabled.Reason != "element() wraps around: index 5 is past the end of the list of 2" || len(a.Edit.Changes) != 0 {
		t.Errorf("expected a disabled action, got %#v", a)
	}

	a = actionTitled(t, ls.codeActions(t, dir, "main.tf", at("lookup"), []string{"refactor.rewrite"}), `Replace lookup() with var.ports["web"]`)
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{"main.tf": strings.Replace(files["main.tf"], `lookup(var.ports, "web")`, `var.ports["web"]`, 1)})

	a = actionTitled(t, ls.codeActions(t, dir, "main.tf", at(`"random_pet"`), nil), `Add "random" to required_providers in a new versions.tf`)
	expectFiles(t, applyAction(t, dir, files, a), map[string]string{"versions.tf": "terraform {\n  required_providers {\n    random = {\n      source = \"hashicorp/random\"\n    }\n  }\n}\n"})

	// quick fixes only: no rewrites
	if actions := ls.codeActions(t, dir, "main.tf", at("lookup"), []string{"quickfix"}); len(actions) != 0 {
		t.Errorf("expected no actions for quickfix, got %#v", actions)
	}
	// an ordinary position with no diagnostics: nothing, and no formatting
	// unless asked for
	if actions := ls.codeActions(t, dir, "main.tf", at("zones"), nil); len(actions) != 0 {
		t.Errorf("expected no actions, got %#v", actions)
	}
}
