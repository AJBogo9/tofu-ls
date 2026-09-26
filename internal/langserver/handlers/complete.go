// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) TextDocumentComplete(ctx context.Context, params lsp.CompletionParams) (lsp.CompletionList, error) {
	var list lsp.CompletionList

	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return list, err
	}

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return list, err
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return list, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	d, err := svc.decoderForDocument(ctx, doc)
	if err != nil {
		return list, err
	}

	expFeatures, err := lsctx.ExperimentalFeatures(ctx)
	if err != nil {
		return list, err
	}

	d.PrefillRequiredFields = expFeatures.PrefillRequiredFields

	pos, err := ilsp.HCLPositionFromLspPosition(params.TextDocumentPositionParams.Position, doc)
	if err != nil {
		return list, err
	}

	if pathList, ok := svc.filePathCompletion(doc, pos); ok {
		return pathList, nil
	}

	svc.logger.Printf("Looking for candidates at %q -> %#v", doc.Filename, pos)
	candidates, err := d.CompletionAtPos(ctx, doc.Filename, pos)
	svc.logger.Printf("received candidates: %#v", candidates)
	commands, entry := svc.providerCompletionExtras(ctx, doc, pos, &candidates)
	if entry && err != nil {
		// a half-typed required_providers entry breaks its block
		candidates.IsComplete, err = true, nil
	}
	list = ilsp.ToCompletionList(candidates, cc.TextDocument)
	for i, command := range commands {
		list.Items[i].Command = command
	}
	return list, err
}
