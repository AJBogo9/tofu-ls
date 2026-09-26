// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"time"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) TextDocumentDidChange(ctx context.Context, params lsp.DidChangeTextDocumentParams) error {
	p := lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{
				URI: params.TextDocument.URI,
			},
			Version: params.TextDocument.Version,
		},
		ContentChanges: params.ContentChanges,
	}

	dh := ilsp.HandleFromDocumentURI(p.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return err
	}

	docCtx := lsctx.DocumentContext(ctx)
	docCtx.LanguageID = doc.LanguageID
	ctx = lsctx.WithDocumentContext(ctx, docCtx)

	newVersion := int(p.TextDocument.Version)

	// Versions don't have to be consecutive, but they must be increasing
	if newVersion <= doc.Version {
		svc.logger.Printf("Old document version (%d) received, current version is %d. "+
			"Ignoring this update for %s. This is likely a client bug, please report it.",
			newVersion, doc.Version, p.TextDocument.URI)
		return nil
	}

	changes := ilsp.DocumentChanges(params.ContentChanges)
	newText, err := document.ApplyChanges(doc.Text, changes)
	if err != nil {
		return err
	}
	err = svc.stateStore.DocumentStore.UpdateDocument(dh, newText, newVersion)
	if err != nil {
		return err
	}

	svc.eventBus.DidChange(eventbus.DidChangeEvent{
		Context:    ctx, // We pass the context for data here
		Dir:        dh.Dir,
		LanguageID: string(ilsp.ParseLanguageID(doc.LanguageID)),
	})

	svc.scheduleInlayHintRefresh()

	return nil
}

// inlayHintRefreshDelay batches the refreshes of a burst of edits.
const inlayHintRefreshDelay = 300 * time.Millisecond

// scheduleInlayHintRefresh asks the client to request inlay hints again
// shortly after an edit. The value hints of one file depend on the other
// files of its module, its tfvars and its callers, which the client does
// not know about, so without a refresh a visible file keeps stale values.
func (svc *service) scheduleInlayHintRefresh() {
	if !svc.inlayHintRefresh || !svc.inlayHints.Values || svc.server == nil {
		return
	}
	svc.inlayRefreshMu.Lock()
	defer svc.inlayRefreshMu.Unlock()
	if svc.inlayRefreshStopped {
		// shutting down: a refresh sent after exit would be answered
		// by a client that is gone
		return
	}
	if svc.inlayRefresh != nil {
		svc.inlayRefresh.Stop()
	}
	svc.inlayRefresh = time.AfterFunc(inlayHintRefreshDelay, func() {
		if svc.sessCtx.Err() != nil {
			return
		}
		if _, err := svc.server.Callback(svc.sessCtx, "workspace/inlayHint/refresh", nil); err != nil {
			svc.logger.Printf("refreshing inlay hints: %s", err)
		}
	})
}

// stopInlayHintRefresh stops a pending inlay hint refresh for good, on
// shutdown.
func (svc *service) stopInlayHintRefresh() {
	svc.inlayRefreshMu.Lock()
	defer svc.inlayRefreshMu.Unlock()
	svc.inlayRefreshStopped = true
	if svc.inlayRefresh != nil {
		svc.inlayRefresh.Stop()
		svc.inlayRefresh = nil
	}
}
