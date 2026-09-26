// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/uri"
)

func TestRefTargetsToLocationLinksInText(t *testing.T) {
	// main.tf: `/* 🚀 */ locals {` on line 1, with the local after the
	// emoji; the reference is in the same file
	src := "/* \U0001F680 */ locals { a = 1 }\noutput \"o\" { value = \"\U0001F680${local.a}\" }\n"
	pos := func(line, col, byte int) hcl.Pos { return hcl.Pos{Line: line, Column: col, Byte: byte} }
	dir := t.TempDir()
	reads := map[string]int{}
	readFile := func(path string) ([]byte, error) {
		reads[path]++
		if path == dir+"/main.tf" {
			return []byte(src), nil
		}
		return nil, fmt.Errorf("unexpected read of %s", path)
	}

	targets := decoder.ReferenceTargets{
		{
			// local.a: columns count the emoji as one, bytes as four
			OriginRange: hcl.Range{Filename: "main.tf", Start: pos(2, 26, 56), End: pos(2, 33, 63)},
			Path:        lang.Path{Path: dir},
			Range:       hcl.Range{Filename: "main.tf", Start: pos(1, 18, 20), End: pos(1, 23, 25)},
			DefRangePtr: &hcl.Range{Filename: "main.tf", Start: pos(1, 18, 20), End: pos(1, 19, 21)},
		},
		{
			// a path argument's file: its start needs no text, so the
			// file is never read
			OriginRange: hcl.Range{Filename: "main.tf", Start: pos(2, 1, 28), End: pos(2, 7, 34)},
			Path:        lang.Path{Path: dir},
			Range:       hcl.Range{Filename: "big.bin", Start: hcl.InitialPos, End: hcl.InitialPos},
		},
	}
	rng := func(startLine, startChar, endLine, endChar uint32) lsp.Range {
		return lsp.Range{Start: lsp.Position{Line: startLine, Character: startChar}, End: lsp.Position{Line: endLine, Character: endChar}}
	}
	origin := rng(1, 26, 1, 33)
	fileOrigin := rng(1, 0, 1, 6)
	want := []lsp.LocationLink{
		{
			OriginSelectionRange: &origin,
			TargetURI:            lsp.DocumentURI(uri.FromPath(dir + "/main.tf")),
			TargetRange:          rng(0, 18, 0, 23),
			TargetSelectionRange: rng(0, 18, 0, 19),
		},
		{
			OriginSelectionRange: &fileOrigin,
			TargetURI:            lsp.DocumentURI(uri.FromPath(dir + "/big.bin")),
			TargetRange:          rng(0, 0, 0, 0),
			TargetSelectionRange: rng(0, 0, 0, 0),
		},
	}
	got := RefTargetsToLocationLinksInText(targets, true, dir+"/main.tf", readFile)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected links: %s", diff)
	}
	if diff := cmp.Diff(map[string]int{dir + "/main.tf": 1}, reads); diff != "" {
		t.Fatalf("unexpected reads: %s", diff)
	}
}
