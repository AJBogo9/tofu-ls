// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"bytes"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/document"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

func documentRangeToLSP(docRng *document.Range) lsp.Range {
	if docRng == nil {
		return lsp.Range{}
	}

	return lsp.Range{
		Start: lsp.Position{
			Character: uint32(docRng.Start.Column),
			Line:      uint32(docRng.Start.Line),
		},
		End: lsp.Position{
			Character: uint32(docRng.End.Column),
			Line:      uint32(docRng.End.Line),
		},
	}
}

func lspRangeToDocRange(rng *lsp.Range) *document.Range {
	if rng == nil {
		return nil
	}

	return &document.Range{
		Start: document.Pos{
			Line:   int(rng.Start.Line),
			Column: int(rng.Start.Character),
		},
		End: document.Pos{
			Line:   int(rng.End.Line),
			Column: int(rng.End.Character),
		},
	}
}

func HCLRangeToLSP(rng hcl.Range) lsp.Range {
	return lsp.Range{
		Start: HCLPosToLSP(rng.Start),
		End:   HCLPosToLSP(rng.End),
	}
}

func HCLPosToLSP(pos hcl.Pos) lsp.Position {
	return lsp.Position{
		Line:      uint32(pos.Line - 1),
		Character: uint32(pos.Column - 1),
	}
}

// HCLRangeToLSPInText converts rng like HCLRangeToLSP, but counts the
// characters in UTF-16 code units of text, the file the range is in, as
// LSP requires. HCL counts columns in grapheme clusters, so the two differ
// on lines with characters outside the Basic Multilingual Plane (such as
// emoji) or with combining marks.
func HCLRangeToLSPInText(rng hcl.Range, text []byte) lsp.Range {
	return lsp.Range{
		Start: HCLPosToLSPInText(rng.Start, text),
		End:   HCLPosToLSPInText(rng.End, text),
	}
}

// HCLPosToLSPInText converts pos like HCLPosToLSP, counting the character
// in UTF-16 code units from the byte offset. It falls back to the column
// when text does not match the position, for example when it is stale.
func HCLPosToLSPInText(pos hcl.Pos, text []byte) lsp.Position {
	if pos.Byte < 0 || pos.Byte > len(text) || bytes.Count(text[:pos.Byte], []byte("\n")) != pos.Line-1 {
		return HCLPosToLSP(pos)
	}
	lineStart := bytes.LastIndexByte(text[:pos.Byte], '\n') + 1
	units := 0
	for rest := text[lineStart:pos.Byte]; len(rest) > 0; {
		r, size := utf8.DecodeRune(rest)
		rest = rest[size:]
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	return lsp.Position{
		Line:      uint32(pos.Line - 1),
		Character: uint32(units),
	}
}
