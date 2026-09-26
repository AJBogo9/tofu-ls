// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/opentofu/tofu-ls/internal/document"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) TextDocumentSemanticTokensFull(ctx context.Context, params lsp.SemanticTokensParams) (lsp.SemanticTokens, error) {
	tks := lsp.SemanticTokens{}

	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return tks, err
	}

	caps := ilsp.SemanticTokensClientCapabilities{
		SemanticTokensClientCapabilities: cc.TextDocument.SemanticTokens,
	}
	if !caps.FullRequest() {
		// This would indicate a buggy client which sent a request
		// it didn't claim to support, so we just strictly follow
		// the protocol here and avoid serving buggy clients.
		svc.logger.Printf("semantic tokens full request support not announced by client")
		return tks, jrpc2.MethodNotFound.Err()
	}

	doc, tokens, err := svc.semanticTokensForDocument(ctx, params.TextDocument.URI, 0, 0)
	if err != nil {
		return tks, err
	}

	te := &ilsp.TokenEncoder{
		Lines:      doc.Lines,
		Tokens:     tokens,
		ClientCaps: cc.TextDocument.SemanticTokens,
	}
	tks.Data = te.Encode()

	return tks, nil
}

// TextDocumentSemanticTokensRange returns the tokens on the lines of the
// requested range, so that a client can color the visible part of a large
// file before the full result arrives.
func (svc *service) TextDocumentSemanticTokensRange(ctx context.Context, params lsp.SemanticTokensRangeParams) (lsp.SemanticTokens, error) {
	tks := lsp.SemanticTokens{}

	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return tks, err
	}

	caps := ilsp.SemanticTokensClientCapabilities{
		SemanticTokensClientCapabilities: cc.TextDocument.SemanticTokens,
	}
	if !caps.RangeRequest() {
		svc.logger.Printf("semantic tokens range request support not announced by client")
		return tks, jrpc2.MethodNotFound.Err()
	}

	startLine, endLine := int(params.Range.Start.Line)+1, int(params.Range.End.Line)+1
	doc, tokens, err := svc.semanticTokensForDocument(ctx, params.TextDocument.URI, startLine, endLine)
	if err != nil {
		return tks, err
	}

	te := &ilsp.TokenEncoder{
		Lines:      doc.Lines,
		Tokens:     tokensOnLines(tokens, startLine, endLine),
		ClientCaps: cc.TextDocument.SemanticTokens,
	}
	tks.Data = te.Encode()

	return tks, nil
}

// semanticTokensForDocument decodes the tokens of a document: all of them
// when startLine is 0, else those of the top-level blocks and attributes
// which touch the (1-based, inclusive) lines.
func (svc *service) semanticTokensForDocument(ctx context.Context, uri lsp.DocumentURI, startLine, endLine int) (*document.Document, []lang.SemanticToken, error) {
	dh := ilsp.HandleFromDocumentURI(uri)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, nil, err
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	d, err := svc.decoderForDocument(ctx, doc)
	if err != nil {
		return nil, nil, err
	}

	var tokens []lang.SemanticToken
	if startLine > 0 {
		tokens, err = d.SemanticTokensInLines(ctx, doc.Filename, startLine, endLine)
	} else {
		tokens, err = d.SemanticTokensInFile(ctx, doc.Filename)
	}
	if err != nil {
		return nil, nil, err
	}

	return doc, tokens, nil
}

// tokensOnLines returns the tokens which touch any of the (1-based,
// inclusive) lines from startLine to endLine, in their original order.
func tokensOnLines(tokens []lang.SemanticToken, startLine, endLine int) []lang.SemanticToken {
	inRange := make([]lang.SemanticToken, 0)
	for _, token := range tokens {
		if token.Range.End.Line < startLine || token.Range.Start.Line > endLine {
			continue
		}
		inRange = append(inRange, token)
	}
	return inRange
}
