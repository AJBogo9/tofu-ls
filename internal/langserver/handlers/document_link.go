// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) TextDocumentLink(ctx context.Context, params lsp.DocumentLinkParams) ([]lsp.DocumentLink, error) {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return nil, err
	}

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return nil, err
	}

	if doc.LanguageID != ilsp.OpenTofu.String() && !ilsp.IsValidTerragruntLanguage(doc.LanguageID) {
		return nil, nil
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return nil, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	if ilsp.IsValidTerragruntLanguage(doc.LanguageID) {
		return ilsp.Links(svc.terragruntLinks(doc), cc.TextDocument.DocumentLink), nil
	}

	d, err := svc.decoderForDocument(ctx, doc)
	if err != nil {
		return nil, err
	}

	links, err := d.LinksInFile(doc.Filename)
	// paths of files that functions read need no schema
	links = append(links, svc.filePathLinks(ctx, doc)...)
	if err != nil && len(links) == 0 {
		return nil, err
	}

	return ilsp.Links(links, cc.TextDocument.DocumentLink), nil
}
