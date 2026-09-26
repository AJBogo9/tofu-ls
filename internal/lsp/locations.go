// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"path/filepath"

	"github.com/hashicorp/hcl-lang/decoder"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func RefOriginsToLocations(origins decoder.ReferenceOrigins) []lsp.Location {
	locations := make([]lsp.Location, len(origins))

	for i, origin := range origins {
		originUri := uri.FromPath(filepath.Join(origin.Path.Path, origin.Range.Filename))
		locations[i] = lsp.Location{
			URI:   lsp.DocumentURI(originUri),
			Range: HCLRangeToLSP(origin.Range),
		}
	}

	return locations
}

// RefOriginsToLocationsInText is RefOriginsToLocations, counting the
// characters of each range in UTF-16 code units of the origin's file, as
// LSP requires. readFile reads a file by its path, each file once.
func RefOriginsToLocationsInText(origins decoder.ReferenceOrigins, readFile func(path string) ([]byte, error)) []lsp.Location {
	locations := make([]lsp.Location, len(origins))
	texts := make(map[string][]byte)

	for i, origin := range origins {
		path := filepath.Join(origin.Path.Path, origin.Range.Filename)
		text, ok := texts[path]
		if !ok {
			// without the text, the range falls back to columns
			text, _ = readFile(path)
			texts[path] = text
		}
		locations[i] = lsp.Location{
			URI:   lsp.DocumentURI(uri.FromPath(path)),
			Range: HCLRangeToLSPInText(origin.Range, text),
		}
	}

	return locations
}
