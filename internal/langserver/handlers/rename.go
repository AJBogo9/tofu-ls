// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hashicorp/hcl-lang/lang"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/refactor"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func (svc *service) PrepareRename(ctx context.Context, params lsp.PrepareRenameParams) (*lsp.PrepareRenameResult, error) {
	sym, err := svc.symbolForRename(ctx, params.TextDocument.URI, params.Position)
	if err != nil {
		if errors.Is(err, refactor.ErrNotRenamable) {
			// null tells the client that the position cannot be renamed
			return nil, nil
		}
		return nil, err
	}

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	return &lsp.PrepareRenameResult{
		Range:       ilsp.HCLRangeToLSPInText(sym.CursorRange, doc.Text),
		Placeholder: sym.Name,
	}, nil
}

func (svc *service) Rename(ctx context.Context, params lsp.RenameParams) (*lsp.WorkspaceEdit, error) {
	sym, err := svc.symbolForRename(ctx, params.TextDocument.URI, params.Position)
	if err != nil {
		return nil, err
	}

	edits, err := refactor.Rename(ctx, svc.refactorEnv(), sym, params.NewName, refactor.Options{
		AddMovedBlock: svc.renameOptions.AddMovedBlock,
	})
	if err != nil {
		return nil, err
	}

	changes := make(map[lsp.DocumentURI][]lsp.TextEdit, 0)
	texts := make(map[string][]byte)
	for _, edit := range edits {
		// LSP counts characters in UTF-16 code units, so the edit ranges
		// are converted with the text of their file.
		text, ok := texts[edit.File]
		if !ok {
			text, _ = svc.fs.ReadFile(filepath.Clean(edit.File))
			texts[edit.File] = text
		}
		docURI := lsp.DocumentURI(uri.FromPath(edit.File))
		changes[docURI] = append(changes[docURI], lsp.TextEdit{
			Range:   ilsp.HCLRangeToLSPInText(edit.Range, text),
			NewText: edit.NewText,
		})
	}

	return &lsp.WorkspaceEdit{Changes: changes}, nil
}

func (svc *service) symbolForRename(ctx context.Context, docURI lsp.DocumentURI, position lsp.Position) (*refactor.Symbol, error) {
	dh := ilsp.HandleFromDocumentURI(docURI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)
	svc.decodeWorkspaceModules(ctx)

	pos, err := ilsp.HCLPositionFromLspPosition(position, doc)
	if err != nil {
		return nil, err
	}

	path := lang.Path{
		Path:       doc.Dir.Path(),
		LanguageID: ilsp.ParseLanguageID(doc.LanguageID).String(),
	}

	return refactor.FindSymbol(ctx, svc.refactorEnv(), path, doc.Filename, pos)
}

func (svc *service) refactorEnv() refactor.Env {
	return refactor.Env{
		Decoder:    svc.decoder,
		PathReader: svc.pathReader,
		ReadFile: func(path string) ([]byte, error) {
			return svc.fs.ReadFile(filepath.Clean(path))
		},
		ModuleCalls: svc.features.Modules.DeclaredModuleCalls,
		ReadDir:     svc.fs.ReadDir,
		InWorkspace: svc.workspace.contains,
	}
}

// decodeWorkspaceModules makes sure every module of the workspace is
// decoded, so that references from modules which are not open (such as
// the parent of the open module) are found. Decoding happens once per
// module; later calls return right away.
func (svc *service) decodeWorkspaceModules(ctx context.Context) {
	ids, err := svc.features.Modules.DecodeAllModules(ctx)
	if err != nil {
		svc.logger.Printf("decoding workspace modules: %s", err)
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, ids...)
}
