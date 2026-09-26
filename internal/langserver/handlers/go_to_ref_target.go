// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) GoToDefinition(ctx context.Context, params lsp.TextDocumentPositionParams) (any, error) {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return nil, err
	}

	targets, err := svc.goToReferenceTarget(ctx, params)
	if err != nil {
		return nil, err
	}

	originPath := ilsp.HandleFromDocumentURI(params.TextDocument.URI).FullPath()
	return ilsp.RefTargetsToDefinitionLocationLinksInText(targets, cc.TextDocument.Definition, originPath, svc.fs.ReadFile), nil
}

func (svc *service) GoToDeclaration(ctx context.Context, params lsp.TextDocumentPositionParams) (any, error) {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return nil, err
	}

	targets, err := svc.goToReferenceTarget(ctx, params)
	if err != nil {
		return nil, err
	}

	originPath := ilsp.HandleFromDocumentURI(params.TextDocument.URI).FullPath()
	return ilsp.RefTargetsToDeclarationLocationLinksInText(targets, cc.TextDocument.Declaration, originPath, svc.fs.ReadFile), nil
}

func (svc *service) goToReferenceTarget(ctx context.Context, params lsp.TextDocumentPositionParams) (decoder.ReferenceTargets, error) {
	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	pos, err := ilsp.HCLPositionFromLspPosition(params.Position, doc)
	if err != nil {
		return nil, err
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	if target, ok := svc.filePathTarget(ctx, doc, pos); ok {
		return decoder.ReferenceTargets{target}, nil
	}

	path := lang.Path{
		Path:       doc.Dir.Path(),
		LanguageID: string(ilsp.ParseLanguageID(doc.LanguageID)),
	}

	targets, err := svc.decoder.ReferenceTargetsForOriginAtPos(path, doc.Filename, pos)
	if (err != nil || len(targets) == 0) && ilsp.IsValidTerragruntLanguage(doc.LanguageID) {
		// a path to another file, unless the position is on a reference
		if target, ok := svc.terragruntLinkTarget(doc, pos); ok {
			return decoder.ReferenceTargets{target}, nil
		}
	}
	return targets, err
}
