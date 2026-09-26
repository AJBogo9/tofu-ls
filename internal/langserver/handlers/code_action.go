// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/codeaction"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/langserver/errors"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/refactor"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	"github.com/opentofu/tofu-ls/internal/tofu/module"
	"github.com/opentofu/tofu-ls/internal/uri"
	"github.com/zclconf/go-cty/cty"
)

// initCommand is the command of the vscode-opentofu client which runs
// tofu init for a module directory given as its URI.
const initCommand = "tofu.initCurrent"

func (svc *service) TextDocumentCodeAction(ctx context.Context, params lsp.CodeActionParams) []codeActionItem {
	ca, err := svc.textDocumentCodeAction(ctx, params)
	if err != nil {
		svc.logger.Printf("code action failed: %s", err)
	}

	return codeActionItems(ca)
}

func (svc *service) textDocumentCodeAction(ctx context.Context, params lsp.CodeActionParams) ([]lsp.CodeAction, error) {
	var ca []lsp.CodeAction

	// For action definitions, refer to https://code.visualstudio.com/api/references/vscode-api#CodeActionKind
	// Formatting runs only when the client asks for it by its exact kind,
	// never without an explicit request. Quick fixes and rewrites are
	// what the lightbulb shows, which asks for no particular kind.
	wantFormat := false
	for _, o := range params.Context.Only {
		svc.logger.Printf("Code actions requested: %q", o)
		if o == ilsp.SourceFormatAllTofu {
			wantFormat = true
		}
	}
	wantFix := ilsp.Wants(params.Context.Only, ilsp.QuickFix)
	wantRewrite := ilsp.Wants(params.Context.Only, ilsp.RefactorRewrite)
	wantRefactor := wantsRefactorings(params.Context.Only)
	if !wantFormat && !wantFix && !wantRewrite && !wantRefactor {
		return nil, fmt.Errorf("could not find a supported code action to execute for %s, wanted %v",
			params.TextDocument.URI, params.Context.Only)
	}

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)

	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return ca, err
	}

	if wantFormat {
		tfExec, err := module.TofuExecutorForModule(ctx, dh.Dir.Path())
		if err != nil {
			return ca, errors.EnrichTfExecError(err)
		}

		edits, err := svc.formatDocument(ctx, tfExec, doc.Text, dh)
		if err != nil {
			return ca, err
		}

		ca = append(ca, lsp.CodeAction{
			Title: "Format Document",
			Kind:  ilsp.SourceFormatAllTofu,
			Edit: lsp.WorkspaceEdit{
				Changes: map[lsp.DocumentURI][]lsp.TextEdit{
					lsp.DocumentURI(dh.FullURI()): edits,
				},
			},
		})
	}

	langID := ilsp.ParseLanguageID(doc.LanguageID)
	if langID != ilsp.OpenTofu && langID != ilsp.OpenTofuVars {
		return ca, nil
	}
	cdoc := codeaction.Document{
		Path: filepath.Join(doc.Dir.Path(), doc.Filename),
		Text: doc.Text,
		Vars: langID == ilsp.OpenTofuVars,
	}
	if wantFix {
		ca = append(ca, svc.quickFixes(ctx, doc, cdoc, params.Context.Diagnostics)...)
	}
	if wantRewrite && langID == ilsp.OpenTofu {
		pos, err := ilsp.HCLPositionFromLspPosition(params.Range.Start, doc)
		if err == nil {
			for _, a := range codeaction.Intentions(svc.codeActionEnv(ctx), cdoc, pos) {
				if action, ok := svc.lspCodeAction(ctx, a, nil); ok {
					ca = append(ca, action)
				}
			}
		}
	}
	if wantRefactor && langID == ilsp.OpenTofu {
		ca = append(ca, svc.refactorings(ctx, doc, cdoc, params)...)
	}

	return ca, nil
}

// quickFixes answers the diagnostics of the request which carry a code
// the fixes know. Nothing else runs when there are none, since clients
// ask for code actions on every cursor move.
func (svc *service) quickFixes(ctx context.Context, doc *document.Document, cdoc codeaction.Document, diags []lsp.Diagnostic) []lsp.CodeAction {
	var ca []lsp.CodeAction
	byTitle := map[string]int{}
	var env *codeaction.Env
	for _, d := range diags {
		code, _ := d.Code.(string)
		if code == "" {
			continue
		}
		if env == nil {
			// the fixes read the module's files and schema: let the
			// pending parse and decode jobs finish first
			jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(doc.Dir)
			if err == nil {
				svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)
			}
			e := svc.codeActionEnv(ctx)
			env = &e
		}
		start, err := ilsp.HCLPositionFromLspPosition(d.Range.Start, doc)
		if err != nil {
			continue
		}
		end, err := ilsp.HCLPositionFromLspPosition(d.Range.End, doc)
		if err != nil {
			continue
		}
		data, _ := d.Data.(map[string]interface{})
		diag := codeaction.Diagnostic{
			Code:  code,
			Data:  data,
			Range: hcl.Range{Filename: cdoc.Path, Start: start, End: end},
		}
		for _, a := range codeaction.QuickFixes(*env, cdoc, diag) {
			// one diagnostic per missing attribute, or a diagnostic sent
			// twice, gets one action which fixes them all
			if i, ok := byTitle[a.Title]; ok {
				ca[i].Diagnostics = append(ca[i].Diagnostics, d)
				continue
			}
			if action, ok := svc.lspCodeAction(ctx, a, []lsp.Diagnostic{d}); ok {
				byTitle[a.Title] = len(ca)
				ca = append(ca, action)
			}
		}
	}
	return ca
}

// codeActionEnv connects the code actions to the indexed modules. The
// functions run only when an action needs them.
func (svc *service) codeActionEnv(ctx context.Context) codeaction.Env {
	env := codeaction.Env{
		ReadFile: func(path string) ([]byte, error) {
			return svc.fs.ReadFile(filepath.Clean(path))
		},
		ReadDir: svc.fs.ReadDir,
		DirURI:  uri.FromPath,
	}

	cc, err := ilsp.ClientCapabilities(ctx)
	if err == nil {
		svc.refactoringEnv(ctx, &env, cc)
		if we := cc.Workspace.WorkspaceEdit; we != nil && we.DocumentChanges {
			for _, op := range we.ResourceOperations {
				if op == "create" {
					env.CreateFiles = true
				}
			}
		}
		// The vscode-opentofu client, which registers the init command,
		// announces its own commands to the server this way, whatever
		// the user's settings.
		if _, ok := lsp.ExperimentalClientCapabilities(cc.Experimental).RefreshModuleCallsCommandId(); ok {
			env.InitCommand = initCommand
		}
	}

	if svc.features == nil || svc.features.Modules == nil {
		return env
	}
	modules := svc.features.Modules
	env.Schema = func(dir string) *schema.BodySchema {
		pathCtx, err := modules.PathContext(lang.Path{Path: dir, LanguageID: ilsp.OpenTofu.String()})
		if err != nil {
			return nil
		}
		return pathCtx.Schema
	}
	env.ParsedFile = func(path string, src []byte) (*hclsyntax.Body, bool) {
		rec, err := modules.Store.ModuleRecordByPath(filepath.Dir(path))
		if err != nil {
			return nil, false
		}
		name := ast.ModFilename(filepath.Base(path))
		f, ok := rec.ParsedModuleFiles[name]
		if !ok || f == nil || !bytes.Equal(f.Bytes, src) || rec.ModuleDiagnostics[globalAst.HCLParsingSource][name].HasErrors() {
			return nil, false
		}
		body, ok := f.Body.(*hclsyntax.Body)
		return body, ok
	}
	env.VariableType = func(dir, name string) (cty.Type, bool) {
		rec, err := modules.Store.ModuleRecordByPath(dir)
		if err != nil {
			return cty.NilType, false
		}
		v, ok := rec.Meta.Variables[name]
		if !ok || v.Type == cty.NilType || v.Type == cty.DynamicPseudoType {
			return cty.NilType, false
		}
		return v.Type, true
	}
	env.VariableSettings = func(dir, name string) ([]codeaction.Location, bool) {
		// the module calls of modules which were only discovered are
		// not known yet; this loads them once per module
		if err := modules.DecodeCallersOf(ctx, dir); err != nil {
			svc.logger.Printf("code action: callers of %q: %s", dir, err)
			return nil, false
		}
		settings, err := refactor.VariableSettings(ctx, svc.refactorEnv(), lang.Path{Path: dir, LanguageID: ilsp.OpenTofu.String()}, name)
		if err != nil {
			svc.logger.Printf("code action: settings of variable %q: %s", name, err)
			return nil, false
		}
		locs := make([]codeaction.Location, 0, len(settings))
		for _, s := range settings {
			locs = append(locs, codeaction.Location{File: s.File, Range: s.Range})
		}
		return locs, true
	}
	return env
}

// lspCodeAction converts an action to the protocol. Edit ranges count
// characters in UTF-16 code units of their file's current text.
func (svc *service) lspCodeAction(ctx context.Context, a codeaction.Action, diags []lsp.Diagnostic) (lsp.CodeAction, bool) {
	action := lsp.CodeAction{
		Title:       a.Title,
		Kind:        lsp.CodeActionKind(a.Kind),
		IsPreferred: a.Preferred,
		Diagnostics: diags,
	}
	if a.Disabled != "" {
		cc, err := ilsp.ClientCapabilities(ctx)
		if err != nil || !cc.TextDocument.CodeAction.DisabledSupport {
			return action, false
		}
		action.Disabled = &lsp.PDisabledMsg_textDocument_codeAction{Reason: a.Disabled}
		return action, true
	}

	created := map[string]bool{}
	for _, file := range a.Create {
		created[file] = true
	}
	edits := map[string][]lsp.TextEdit{}
	files := make([]string, 0)
	texts := map[string][]byte{}
	for _, e := range a.Edits {
		text, ok := texts[e.File]
		if !ok && !created[e.File] {
			text, _ = svc.fs.ReadFile(filepath.Clean(e.File))
			texts[e.File] = text
		}
		if _, ok := edits[e.File]; !ok {
			files = append(files, e.File)
		}
		edits[e.File] = append(edits[e.File], lsp.TextEdit{
			Range:   ilsp.HCLRangeToLSPInText(e.Range, text),
			NewText: e.NewText,
		})
	}
	sort.Strings(files)

	if len(a.Create) == 0 {
		if len(files) > 0 {
			action.Edit.Changes = make(map[lsp.DocumentURI][]lsp.TextEdit, len(files))
			for _, file := range files {
				action.Edit.Changes[lsp.DocumentURI(uri.FromPath(file))] = edits[file]
			}
		}
	} else {
		for _, file := range a.Create {
			action.Edit.DocumentChanges = append(action.Edit.DocumentChanges, lsp.DocumentChanges{
				CreateFile: &lsp.CreateFile{
					Kind:    "create",
					URI:     lsp.DocumentURI(uri.FromPath(file)),
					Options: &lsp.CreateFileOptions{IgnoreIfExists: true},
				},
			})
		}
		for _, file := range files {
			action.Edit.DocumentChanges = append(action.Edit.DocumentChanges, lsp.DocumentChanges{
				TextDocumentEdit: &lsp.TextDocumentEdit{
					TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
						TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(uri.FromPath(file))},
					},
					Edits: edits[file],
				},
			})
		}
	}

	if a.Command != nil {
		args := make([]json.RawMessage, 0, len(a.Command.Arguments))
		for _, arg := range a.Command.Arguments {
			raw, err := json.Marshal(arg)
			if err != nil {
				return action, false
			}
			args = append(args, raw)
		}
		action.Command = &lsp.Command{Title: a.Command.Title, Command: a.Command.Name, Arguments: args}
	}
	return action, true
}
