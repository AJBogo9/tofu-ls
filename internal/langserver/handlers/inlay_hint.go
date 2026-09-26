// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/staticval"
)

// TextDocumentInlayHint shows the statically known value after var and
// local references, or of whole expressions (opentofu.inlayHints.values).
func (svc *service) TextDocumentInlayHint(ctx context.Context, params lsp.InlayHintParams) ([]lsp.InlayHint, error) {
	hints := []lsp.InlayHint{}
	if !svc.inlayHints.Values {
		return hints, nil
	}

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}
	if ilsp.ParseLanguageID(doc.LanguageID) != ilsp.OpenTofu {
		return hints, nil
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	var rng hcl.Range
	start, err := ilsp.HCLPositionFromLspPosition(params.Range.Start, doc)
	if err == nil {
		end, err := ilsp.HCLPositionFromLspPosition(params.Range.End, doc)
		if err == nil {
			rng = hcl.Range{Filename: doc.Filename, Start: start, End: end}
		}
	}

	// The decoder's schema tells which arguments the provider marks
	// sensitive; their values get no hints.
	var bodySchema *schema.BodySchema
	if d, err := svc.decoderForDocument(ctx, doc); err == nil {
		bodySchema = d.Schema()
	}
	ev, err := svc.staticEvaluator(ctx, doc.Dir.Path(), bodySchema)
	if err != nil {
		return nil, err
	}
	opts := staticval.InlayOptions{Policy: staticval.InlayInformative, MaxLength: svc.inlayHints.MaxLength}
	if svc.inlayHints.ValuePolicy == "all" {
		opts.Policy = staticval.InlayAll
	} else {
		// a library module's defaults are placeholders its callers replace
		opts.HideDefaults = staticval.IsLibraryRoot(svc.fs, ev.Module(), svc.indexedCallers)
	}
	for _, h := range ev.InlayHintsWith(doc.Filename, rng, opts) {
		pos := ilsp.HCLPosToLSPInText(h.Pos, doc.Text)
		hints = append(hints, lsp.InlayHint{
			Position: &pos,
			Label: []lsp.InlayHintLabelPart{{
				Value: h.Label,
			}},
			PaddingLeft: true,
		})
	}
	return hints, nil
}
