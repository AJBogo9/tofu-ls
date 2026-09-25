// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/refactor"
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
			Range: ilsp.HCLRangeToLSP(rng),
			Kind:  kind,
		})
	}

	for _, target := range targets {
		nameRng, hasName := refactor.DeclarationNameRange(svc.refactorEnv(), target.Path, target.Target)
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

		if target.Path.Equals(path) && target.Target.RangePtr.Filename == doc.Filename {
			if hasName {
				add(nameRng, lsp.Write)
			} else if target.Target.DefRangePtr != nil {
				add(*target.Target.DefRangePtr, lsp.Write)
			}
		}

		for _, origin := range svc.decoder.OriginsTargeting(ctx, target.Target, target.Path) {
			rng := origin.Origin.OriginRange()
			if origin.Path.Equals(path) && rng.Filename == doc.Filename {
				add(rng, lsp.Read)
			}
		}
	}

	if len(highlights) == 0 {
		return nil, nil
	}
	return highlights, nil
}
