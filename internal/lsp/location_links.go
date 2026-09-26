// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"path/filepath"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func RefTargetsToDefinitionLocationLinks(targets decoder.ReferenceTargets, defCaps *lsp.DefinitionClientCapabilities) interface{} {
	if defCaps == nil {
		return RefTargetsToLocationLinks(targets, false)
	}
	return RefTargetsToLocationLinks(targets, defCaps.LinkSupport)
}

func RefTargetsToDeclarationLocationLinks(targets decoder.ReferenceTargets, declCaps *lsp.DeclarationClientCapabilities) interface{} {
	if declCaps == nil {
		return RefTargetsToLocationLinks(targets, false)
	}
	return RefTargetsToLocationLinks(targets, declCaps.LinkSupport)
}

func RefTargetsToLocationLinks(targets decoder.ReferenceTargets, linkSupport bool) interface{} {
	if linkSupport {
		links := make([]lsp.LocationLink, 0)
		for _, target := range targets {
			links = append(links, refTargetToLocationLink(target))
		}
		return links
	}

	locations := make([]lsp.Location, 0)
	for _, target := range targets {
		locations = append(locations, refTargetToLocation(target))
	}
	return locations
}

func refTargetToLocationLink(target *decoder.ReferenceTarget) lsp.LocationLink {
	targetUri := uri.FromPath(filepath.Join(target.Path.Path, target.Range.Filename))
	originRange := HCLRangeToLSP(target.OriginRange)

	locLink := lsp.LocationLink{
		OriginSelectionRange: &originRange,
		TargetURI:            lsp.DocumentURI(targetUri),
		TargetRange:          HCLRangeToLSP(target.Range),
		TargetSelectionRange: HCLRangeToLSP(target.Range),
	}

	if target.DefRangePtr != nil {
		locLink.TargetSelectionRange = HCLRangeToLSP(*target.DefRangePtr)
	}

	return locLink
}

func refTargetToLocation(target *decoder.ReferenceTarget) lsp.Location {
	targetUri := uri.FromPath(filepath.Join(target.Path.Path, target.Range.Filename))

	return lsp.Location{
		URI:   lsp.DocumentURI(targetUri),
		Range: HCLRangeToLSP(target.Range),
	}
}

// RefTargetsToDefinitionLocationLinksInText is
// RefTargetsToDefinitionLocationLinks, counting characters in UTF-16
// code units as LSP requires (see HCLRangeToLSPInText): origin ranges in
// the file at originPath, the document the request is for, and target
// ranges in the target's file. readFile reads a file by its path, each
// file once.
func RefTargetsToDefinitionLocationLinksInText(targets decoder.ReferenceTargets, defCaps *lsp.DefinitionClientCapabilities, originPath string, readFile func(path string) ([]byte, error)) interface{} {
	return RefTargetsToLocationLinksInText(targets, defCaps != nil && defCaps.LinkSupport, originPath, readFile)
}

// RefTargetsToDeclarationLocationLinksInText is
// RefTargetsToDefinitionLocationLinksInText for declarations.
func RefTargetsToDeclarationLocationLinksInText(targets decoder.ReferenceTargets, declCaps *lsp.DeclarationClientCapabilities, originPath string, readFile func(path string) ([]byte, error)) interface{} {
	return RefTargetsToLocationLinksInText(targets, declCaps != nil && declCaps.LinkSupport, originPath, readFile)
}

// RefTargetsToLocationLinksInText is RefTargetsToLocationLinks, counting
// characters as RefTargetsToDefinitionLocationLinksInText does.
func RefTargetsToLocationLinksInText(targets decoder.ReferenceTargets, linkSupport bool, originPath string, readFile func(path string) ([]byte, error)) interface{} {
	texts := make(map[string][]byte)
	rangeIn := func(path string, rng hcl.Range) lsp.Range {
		if rng.Start.Column <= 1 && rng.End.Column <= 1 {
			// line starts need no text, which spares reading a file
			// that a path argument points to
			return HCLRangeToLSP(rng)
		}
		text, ok := texts[path]
		if !ok {
			// without the text, the range falls back to columns
			text, _ = readFile(path)
			texts[path] = text
		}
		return HCLRangeToLSPInText(rng, text)
	}

	if linkSupport {
		links := make([]lsp.LocationLink, 0)
		for _, target := range targets {
			path := filepath.Join(target.Path.Path, target.Range.Filename)
			originRange := rangeIn(originPath, target.OriginRange)

			link := lsp.LocationLink{
				OriginSelectionRange: &originRange,
				TargetURI:            lsp.DocumentURI(uri.FromPath(path)),
				TargetRange:          rangeIn(path, target.Range),
				TargetSelectionRange: rangeIn(path, target.Range),
			}
			if target.DefRangePtr != nil {
				link.TargetSelectionRange = rangeIn(path, *target.DefRangePtr)
			}
			links = append(links, link)
		}
		return links
	}

	locations := make([]lsp.Location, 0)
	for _, target := range targets {
		path := filepath.Join(target.Path.Path, target.Range.Filename)
		locations = append(locations, lsp.Location{
			URI:   lsp.DocumentURI(uri.FromPath(path)),
			Range: rangeIn(path, target.Range),
		})
	}
	return locations
}
