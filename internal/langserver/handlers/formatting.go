// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"time"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/hcl"
	"github.com/opentofu/tofu-ls/internal/langserver/errors"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	"github.com/opentofu/tofu-ls/internal/tofu/module"
)

func (svc *service) TextDocumentFormatting(ctx context.Context, params lsp.DocumentFormattingParams) ([]lsp.TextEdit, error) {
	var edits []lsp.TextEdit

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)

	tfExec, err := module.TofuExecutorForModule(ctx, dh.Dir.Path())
	if err != nil {
		return edits, errors.EnrichTfExecError(err)
	}

	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return edits, err
	}

	edits, err = svc.formatDocument(ctx, tfExec, doc.Text, dh)
	if err != nil {
		return edits, err
	}

	return edits, nil
}

func (svc *service) formatDocument(ctx context.Context, tfExec exec.TofuExecutor, original []byte, dh document.Handle) ([]lsp.TextEdit, error) {
	var edits []lsp.TextEdit

	svc.logger.Printf("formatting document via %q", tfExec.GetExecPath())

	startTime := time.Now()
	formatted, err := tfExec.Format(ctx, original)
	if err != nil {
		svc.logger.Printf("Failed 'tofu fmt' in %s", time.Since(startTime))
		return edits, err
	}
	svc.logger.Printf("Finished 'tofu fmt' in %s", time.Since(startTime))

	changes := hcl.Diff(dh, original, formatted)

	return ilsp.TextEditsFromDocumentChanges(changes), nil
}

// TextDocumentRangeFormatting formats the top-level blocks that the range
// touches, whole, with the formatter of TextDocumentFormatting: the edits
// are what formatting the document gives for those blocks, and nothing
// else changes.
func (svc *service) TextDocumentRangeFormatting(ctx context.Context, params lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, error) {
	var edits []lsp.TextEdit

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)

	tfExec, err := module.TofuExecutorForModule(ctx, dh.Dir.Path())
	if err != nil {
		return edits, errors.EnrichTfExecError(err)
	}

	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return edits, err
	}

	startTime := time.Now()
	formatted, err := tfExec.Format(ctx, doc.Text)
	if err != nil {
		svc.logger.Printf("Failed 'tofu fmt' in %s", time.Since(startTime))
		return edits, err
	}
	svc.logger.Printf("Finished 'tofu fmt' in %s", time.Since(startTime))

	startLine := int(params.Range.Start.Line)
	endLine := int(params.Range.End.Line)
	if params.Range.End.Character == 0 && endLine > startLine {
		// a selection of whole lines ends at the start of the next one
		endLine--
	}
	partial, ok := hcl.FormatLines(doc.Filename, doc.Text, formatted, startLine, endLine)
	if !ok {
		return edits, nil
	}

	changes := hcl.Diff(dh, doc.Text, partial)
	return ilsp.TextEditsFromDocumentChanges(changes), nil
}
