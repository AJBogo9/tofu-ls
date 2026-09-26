// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/refactor"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// DocumentHighlight highlights the symbol under the cursor in the current
// file: its declaration as a write and its references as reads.
func (svc *service) DocumentHighlight(ctx context.Context, params lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, error) {
	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	pos, err := ilsp.HCLPositionFromLspPosition(params.Position, doc)
	if err != nil {
		return nil, err
	}

	path := lang.Path{
		Path:       doc.Dir.Path(),
		LanguageID: ilsp.ParseLanguageID(doc.LanguageID).String(),
	}

	targets, onReference := svc.decoder.SymbolTargetsAtPos(path, doc.Filename, pos)
	if len(targets) == 0 {
		return nil, nil
	}

	highlights := make([]lsp.DocumentHighlight, 0)
	seen := make(map[hcl.Range]bool, 0)
	add := func(rng hcl.Range, kind lsp.DocumentHighlightKind) {
		if seen[rng] {
			return
		}
		seen[rng] = true
		highlights = append(highlights, lsp.DocumentHighlight{
			Range: ilsp.HCLRangeToLSPInText(rng, doc.Text),
			Kind:  kind,
		})
	}

	for _, target := range targets {
		inThisFile := target.Path.Equals(path) && target.Target.RangePtr.Filename == doc.Filename
		var nameRng hcl.Range
		var hasName bool
		if !onReference || inThisFile {
			nameRng, hasName = svc.declarationNameRange(target.Path, target.Target)
		}
		if !onReference {
			// Only the declared name highlights its symbol: on a keyword
			// or inside the body, the editor's word highlight is better.
			if hasName && !(nameRng.ContainsPos(pos) || nameRng.End == pos) {
				continue
			}
			if !hasName && (target.Target.DefRangePtr == nil || !target.Target.DefRangePtr.ContainsPos(pos)) {
				continue
			}
		}

		if inThisFile {
			if hasName {
				add(nameRng, lsp.Write)
			} else if target.Target.DefRangePtr != nil {
				add(*target.Target.DefRangePtr, lsp.Write)
			}
		}

		// Only this module can hold uses in this file, so no other
		// module is read.
		for _, origin := range svc.decoder.OriginsTargetingInPath(target.Target, target.Path, path) {
			rng := origin.Origin.OriginRange()
			if rng.Filename == doc.Filename {
				add(rng, lsp.Read)
			}
		}
	}

	if len(highlights) == 0 {
		return nil, nil
	}
	return highlights, nil
}

// declarationNameRange is refactor.DeclarationNameRange, but it takes
// the declaring file from the module's index, which already parsed it,
// instead of reading and parsing it again on every cursor move.
func (svc *service) declarationNameRange(path lang.Path, target reference.Target) (hcl.Range, bool) {
	if target.RangePtr != nil && svc.features != nil && svc.features.Modules != nil {
		mod, err := svc.features.Modules.Store.ModuleRecordByPath(path.Path)
		if err == nil {
			name := ast.ModFilename(target.RangePtr.Filename)
			if f, ok := mod.ParsedModuleFiles[name]; ok {
				if mod.ModuleDiagnostics[globalAst.HCLParsingSource][name].HasErrors() {
					// as refactor.DeclarationNameRange, which refuses
					// a file with syntax errors
					return hcl.Range{}, false
				}
				return refactor.DeclarationNameRangeInFile(path, target, f)
			}
		}
	}
	return refactor.DeclarationNameRange(svc.refactorEnv(), path, target)
}
