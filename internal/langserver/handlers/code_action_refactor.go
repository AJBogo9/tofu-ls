// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/codeaction"
	"github.com/opentofu/tofu-ls/internal/document"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// codeActionItem is a code action as the server sends it. The protocol
// type always writes its edit, and even an empty one tells the client
// that there is nothing to resolve, so an action whose edit is resolved
// later (it has data) is sent without one.
type codeActionItem lsp.CodeAction

func (a codeActionItem) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(lsp.CodeAction(a))
	if err != nil || a.Data == nil {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "edit")
	return json.Marshal(fields)
}

func codeActionItems(ca []lsp.CodeAction) []codeActionItem {
	if ca == nil {
		return nil
	}
	items := make([]codeActionItem, len(ca))
	for i, a := range ca {
		items[i] = codeActionItem(a)
	}
	return items
}

// refactorKinds are the kinds of the refactorings.
var refactorKinds = []lsp.CodeActionKind{ilsp.Refactor, ilsp.RefactorExtract, ilsp.RefactorInline, ilsp.RefactorRewrite}

func wantsRefactorings(only []lsp.CodeActionKind) bool {
	for _, kind := range refactorKinds {
		if ilsp.Wants(only, kind) {
			return true
		}
	}
	return false
}

// refactorData is the data of a refactoring's code action, which the
// client sends back to resolve it.
type refactorData struct {
	Refactoring string    `json:"refactoring"`
	URI         string    `json:"uri"`
	Version     int       `json:"version"`
	Range       lsp.Range `json:"range"`
}

// refactorings offers the refactorings for the range of the request.
// Unless the user asked for code actions, only a selected expression
// gets offers. Their edits are computed when the client resolves them;
// a client which cannot resolve edits gets them at once.
func (svc *service) refactorings(ctx context.Context, doc *document.Document, cdoc codeaction.Document, params lsp.CodeActionParams) []lsp.CodeAction {
	start, err := ilsp.HCLPositionFromLspPosition(params.Range.Start, doc)
	if err != nil {
		return nil
	}
	end, err := ilsp.HCLPositionFromLspPosition(params.Range.End, doc)
	if err != nil || end.Byte < start.Byte {
		return nil
	}
	invoked := params.Context.TriggerKind == lsp.CodeActionInvoked
	for _, o := range params.Context.Only {
		// asking for refactorings is asking
		if strings.HasPrefix(string(o), ilsp.Refactor) {
			invoked = true
		}
	}
	if !invoked && start.Byte == end.Byte {
		// a cursor move offers nothing
		return nil
	}
	env := svc.codeActionEnv(ctx)
	rng := hcl.Range{Filename: cdoc.Path, Start: start, End: end}
	later := resolvesEdits(ctx)

	var ca []lsp.CodeAction
	for _, a := range codeaction.Refactorings(env, cdoc, rng, invoked) {
		if !ilsp.Wants(params.Context.Only, lsp.CodeActionKind(a.Kind)) {
			continue
		}
		ref := a.Resolve
		if a.Disabled == "" && !later {
			resolved, err := codeaction.Resolve(env, cdoc, *ref)
			if err != nil {
				a.Disabled = err.Error()
			} else {
				resolved.Title, resolved.Kind = a.Title, a.Kind
				a = resolved
			}
		}
		action, ok := svc.lspCodeAction(ctx, a, nil)
		if !ok {
			continue
		}
		if a.Disabled == "" && later {
			action.Data = refactorData{
				Refactoring: ref.ID,
				URI:         string(params.TextDocument.URI),
				Version:     doc.Version,
				Range:       ilsp.HCLRangeToLSPInText(ref.Range, doc.Text),
			}
		}
		ca = append(ca, action)
	}
	return ca
}

// resolvesEdits tells whether the client resolves the edits of code
// actions later.
func resolvesEdits(ctx context.Context) bool {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil || cc.TextDocument.CodeAction.ResolveSupport == nil {
		return false
	}
	for _, p := range cc.TextDocument.CodeAction.ResolveSupport.Properties {
		if p == "edit" {
			return true
		}
	}
	return false
}

// CodeActionResolve computes the edits of a refactoring. Other actions
// come back unchanged. A refactoring refused now tells the user why and
// comes back without an edit.
func (svc *service) CodeActionResolve(ctx context.Context, action lsp.CodeAction) (lsp.CodeAction, error) {
	raw, err := json.Marshal(action.Data)
	if err != nil || action.Data == nil {
		return action, nil
	}
	var data refactorData
	if err := json.Unmarshal(raw, &data); err != nil || data.Refactoring == "" {
		return action, nil
	}

	dh := ilsp.HandleFromDocumentURI(lsp.DocumentURI(data.URI))
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return action, err
	}
	if doc.Version != data.Version {
		return action, fmt.Errorf("%s changed after the refactoring was offered", doc.Filename)
	}
	start, err := ilsp.HCLPositionFromLspPosition(data.Range.Start, doc)
	if err != nil {
		return action, err
	}
	end, err := ilsp.HCLPositionFromLspPosition(data.Range.End, doc)
	if err != nil {
		return action, err
	}
	// the refactorings read the module's files and values: let the
	// pending parse and decode jobs finish first
	if ids, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(doc.Dir); err == nil {
		svc.stateStore.JobStore.WaitForJobs(ctx, ids...)
	}

	cdoc := codeaction.Document{Path: filepath.Join(doc.Dir.Path(), doc.Filename), Text: doc.Text}
	ref := codeaction.Refactoring{ID: data.Refactoring, Range: hcl.Range{Filename: cdoc.Path, Start: start, End: end}}
	a, err := codeaction.Resolve(svc.codeActionEnv(ctx), cdoc, ref)
	if err != nil {
		var refusal codeaction.Refusal
		if !errors.As(err, &refusal) {
			return action, err
		}
		svc.logger.Printf("refactoring %q refused: %s", data.Refactoring, refusal)
		jrpc2.ServerFromContext(ctx).Notify(ctx, "window/showMessage", &lsp.ShowMessageParams{
			Type:    lsp.Warning,
			Message: fmt.Sprintf("%s: %s.", action.Title, refusal),
		})
		action.Data = nil
		return action, nil
	}
	resolved, ok := svc.lspCodeAction(ctx, a, action.Diagnostics)
	if !ok {
		return action, nil
	}
	resolved.Title, resolved.Kind, resolved.IsPreferred = action.Title, action.Kind, action.IsPreferred
	return resolved, nil
}

// refactoringEnv connects the refactorings to the client's rename
// command, the module calls and the static evaluator.
func (svc *service) refactoringEnv(ctx context.Context, env *codeaction.Env, cc lsp.ClientCapabilities) {
	env.FileURI = uri.FromPath
	if id, ok := lsp.ExperimentalClientCapabilities(cc.Experimental).RenameCommandId(); ok {
		env.RenameCommand = id
	}
	if svc.features == nil || svc.features.Modules == nil {
		return
	}
	modules := svc.features.Modules
	indexed := func(dir string) []codeaction.ModuleCaller {
		records, err := modules.Store.List()
		if err != nil {
			return nil
		}
		var callers []codeaction.ModuleCaller
		for _, rec := range records {
			for name, mc := range rec.Meta.ModuleCalls {
				src := mc.RawSourceAddr
				if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
					continue
				}
				if filepath.Clean(filepath.Join(rec.Path(), src)) == filepath.Clean(dir) {
					callers = append(callers, codeaction.ModuleCaller{Dir: rec.Path(), Name: name})
				}
			}
		}
		sort.Slice(callers, func(i, j int) bool {
			if callers[i].Dir != callers[j].Dir {
				return callers[i].Dir < callers[j].Dir
			}
			return callers[i].Name < callers[j].Name
		})
		return callers
	}
	env.IndexedCallers = indexed
	env.ModuleCallers = func(dir string) ([]codeaction.ModuleCaller, bool) {
		// the module calls of modules which were only discovered are
		// not known yet; this loads them once per module
		if err := modules.DecodeCallersOf(ctx, dir); err != nil {
			svc.logger.Printf("code action: callers of %q: %s", dir, err)
			return nil, false
		}
		return indexed(dir), true
	}
	env.StaticValue = func(dir string, expr hcl.Expression) codeaction.StaticValue {
		ev, err := svc.staticEvaluator(ctx, dir, nil)
		if err != nil {
			return codeaction.StaticValue{Reason: err.Error()}
		}
		r := ev.Eval(expr, nil)
		return codeaction.StaticValue{
			Value:   r.Value,
			Known:   r.IsKnown(),
			Reason:  r.Reason,
			Sources: valueSources(ev, expr, 0),
		}
	}
}

// valueSources names where the variables an expression reads (directly
// or through local values) get their values: a tfvars file, "default",
// or the module calls of a child module.
func valueSources(ev *staticval.Evaluator, expr hcl.Expression, depth int) []string {
	seen := map[string]bool{}
	var sources []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			sources = append(sources, s)
		}
	}
	for _, tr := range expr.Variables() {
		if len(tr) < 2 {
			continue
		}
		attr, ok := tr[1].(hcl.TraverseAttr)
		if !ok {
			continue
		}
		switch tr.RootName() {
		case "var":
			v, ok := ev.Variable(attr.Name)
			if !ok {
				continue
			}
			if len(v.CallValues) > 0 {
				for _, cv := range v.CallValues {
					add(cv.Call)
				}
				continue
			}
			if _, source, ok := v.Effective(); ok {
				add(source)
			}
		case "local":
			l, ok := ev.Local(attr.Name)
			if ok && depth < 3 {
				for _, s := range valueSources(ev, l.Expr, depth+1) {
					add(s)
				}
			}
		}
	}
	return sources
}
