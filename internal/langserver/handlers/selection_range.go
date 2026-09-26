// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"strings"

	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/selection"
)

func (svc *service) SelectionRange(ctx context.Context, params lsp.SelectionRangeParams) ([]lsp.SelectionRange, error) {
	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	result := make([]lsp.SelectionRange, 0, len(params.Positions))
	for _, position := range params.Positions {
		pos, err := ilsp.HCLPositionFromLspPosition(position, doc)
		if err != nil {
			return nil, err
		}

		var ranges []lsp.Range
		if !strings.HasSuffix(doc.Filename, ".json") {
			for _, rng := range selection.Ranges(doc.Text, doc.Filename, pos) {
				ranges = append(ranges, ilsp.HCLRangeToLSPInText(rng, doc.Text))
			}
		}
		if len(ranges) == 0 {
			// the result has one entry per position, so fall back
			// to the empty range at the position itself
			result = append(result, lsp.SelectionRange{Range: lsp.Range{Start: position, End: position}})
			continue
		}

		// link from the outermost range inwards
		var parent *lsp.SelectionRange
		for i := len(ranges) - 1; i >= 0; i-- {
			parent = &lsp.SelectionRange{Range: ranges[i], Parent: parent}
		}
		result = append(result, *parent)
	}

	return result, nil
}
