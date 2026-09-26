// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"

	"github.com/hashicorp/hcl-lang/lang"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) TextDocumentCodeLens(ctx context.Context, params lsp.CodeLensParams) ([]lsp.CodeLens, error) {
	list := make([]lsp.CodeLens, 0)

	dh := ilsp.HandleFromDocumentURI(params.TextDocument.URI)
	doc, err := svc.stateStore.DocumentStore.GetDocument(dh)
	if err != nil {
		return list, err
	}
	if ilsp.IsValidTerragruntLanguage(doc.LanguageID) {
		// Other Terragrunt files read a file's values (include with expose,
		// read_terragrunt_config), so a count of the uses in the file
		// itself would undercount, and "0 references" would be wrong.
		return list, nil
	}

	jobIds, err := svc.stateStore.JobStore.ListIncompleteJobsForDir(dh.Dir)
	if err != nil {
		return list, err
	}
	svc.stateStore.JobStore.WaitForJobs(ctx, jobIds...)

	path := lang.Path{
		Path:       doc.Dir.Path(),
		LanguageID: string(ilsp.ParseLanguageID(doc.LanguageID)),
	}

	svc.decodeCallersForLenses(ctx, path)

	lenses, err := svc.decoder.CodeLensesForFile(ctx, path, doc.Filename)
	if err != nil {
		return nil, err
	}

	for _, lens := range lenses {
		cmd, err := ilsp.Command(lens.Command)
		if err != nil {
			svc.logger.Printf("skipping code lens %#v: %s", lens.Command, err)
			continue
		}

		list = append(list, lsp.CodeLens{
			Range:   ilsp.HCLRangeToLSP(lens.Range),
			Command: cmd,
		})
	}

	return list, nil
}

// decodeCallersForLenses decodes the modules which call the module of
// path, so that a reference count lens counts the uses in its callers
// from the first request, as the Find References that a click on it
// sends lists them (References decodes the whole workspace).
func (svc *service) decodeCallersForLenses(ctx context.Context, path lang.Path) {
	if svc.features == nil || svc.features.Modules == nil || ilsp.ParseLanguageID(path.LanguageID) != ilsp.OpenTofu {
		return
	}
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return
	}
	if _, ok := lsp.ExperimentalClientCapabilities(cc.Experimental).ShowReferencesCommandId(); !ok {
		// no reference count lenses
		return
	}
	if err := svc.features.Modules.DecodeCallersOf(ctx, path.Path); err != nil {
		svc.logger.Printf("decoding the callers of %q for lenses: %s", path.Path, err)
	}
}
