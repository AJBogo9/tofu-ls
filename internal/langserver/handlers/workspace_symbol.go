// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl-lang/lang"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func (svc *service) WorkspaceSymbol(ctx context.Context, params lsp.WorkspaceSymbolParams) ([]lsp.SymbolInformation, error) {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil {
		return nil, err
	}

	// TODO? maybe kick off indexing of the whole workspace here, use ProgressToken
	symbols, err := svc.decoder.SymbolsInPaths(ctx, params.Query, isInstalledModulePath)
	if err != nil {
		return nil, err
	}

	return ilsp.WorkspaceSymbols(symbols, cc.Workspace.Symbol), nil
}

// isInstalledModulePath tells whether the path is a copy of a module that
// tofu init installed under .terraform, whose symbols (and those of its
// own examples) would bury the workspace's own in search results.
func isInstalledModulePath(path lang.Path) bool {
	sep := string(filepath.Separator)
	return strings.Contains(filepath.Clean(path.Path)+sep, sep+".terraform"+sep)
}
