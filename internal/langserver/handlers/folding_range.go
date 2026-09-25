// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"strings"

	"github.com/opentofu/tofu-ls/internal/folding"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) FoldingRange(ctx context.Context, params lsp.FoldingRangeParams) ([]lsp.FoldingRange, error) {
	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(doc.Filename, ".json") {
		// JSON configuration is not HCL native syntax
		return nil, nil
	}

	return folding.Ranges(doc.Text, doc.Filename), nil
}
