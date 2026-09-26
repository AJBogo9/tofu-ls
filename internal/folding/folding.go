// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package folding computes folding ranges from the HCL syntax tree:
// blocks, multi-line collections, function calls, heredocs and runs
// of comments.
package folding

import (
	"bytes"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
)

// Ranges returns the folding ranges of an HCL native syntax file.
// Lines are zero-based, as in LSP. A range ends on the line before a
// closing brace, bracket or heredoc marker, so that line stays visible
// when the range is folded.
func Ranges(src []byte, filename string) []lsp.FoldingRange {
	c := &collector{src: src, seen: map[[2]uint32]bool{}}

	f, _ := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if f != nil {
		if body, ok := f.Body.(*hclsyntax.Body); ok {
			hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
				c.node(node)
				return nil
			})
		}
	}

	c.comments(filename)

	sort.SliceStable(c.ranges, func(i, j int) bool {
		if c.ranges[i].StartLine != c.ranges[j].StartLine {
			return c.ranges[i].StartLine < c.ranges[j].StartLine
		}
		return c.ranges[i].EndLine > c.ranges[j].EndLine
	})

	// Clients fold by start line, so keep the outermost range of each,
	// e.g. merge({ on one line opens a call and an object.
	ranges := make([]lsp.FoldingRange, 0, len(c.ranges))
	for i, r := range c.ranges {
		if i > 0 && r.StartLine == c.ranges[i-1].StartLine {
			continue
		}
		ranges = append(ranges, r)
	}
	return ranges
}

type collector struct {
	src    []byte
	ranges []lsp.FoldingRange
	seen   map[[2]uint32]bool
}

func (c *collector) node(node hclsyntax.Node) {
	switch n := node.(type) {
	case *hclsyntax.Block:
		c.closed(n.OpenBraceRange.Start.Line, n.CloseBraceRange.Start, "")
	case *hclsyntax.ObjectConsExpr:
		c.closed(n.OpenRange.Start.Line, lastChar(n.SrcRange), "")
	case *hclsyntax.TupleConsExpr:
		c.closed(n.OpenRange.Start.Line, lastChar(n.SrcRange), "")
	case *hclsyntax.ForExpr:
		c.closed(n.OpenRange.Start.Line, n.CloseRange.Start, "")
	case *hclsyntax.FunctionCallExpr:
		c.closed(n.OpenParenRange.Start.Line, n.CloseParenRange.Start, "")
	case *hclsyntax.TemplateExpr:
		rng := n.SrcRange
		if rng.Start.Byte+2 <= len(c.src) && bytes.HasPrefix(c.src[rng.Start.Byte:], []byte("<<")) {
			// the closing marker is the last line of a heredoc
			c.add(rng.Start.Line, rng.End.Line-1, "")
		}
	}
}

// lastChar is the position of the closing delimiter of a range.
func lastChar(rng hcl.Range) hcl.Pos {
	return hcl.Pos{Line: rng.End.Line, Column: rng.End.Column - 1, Byte: rng.End.Byte - 1}
}

// closed adds a range which ends with a closing delimiter at end.
// The closing line stays visible when it holds nothing but closing
// delimiters (and indentation) before it, as in the }) that ends
// merge(var.x, { ... }).
func (c *collector) closed(startLine int, end hcl.Pos, kind string) {
	endLine := end.Line
	if end.Byte <= len(c.src) && onlyClosersBefore(c.src, end.Byte) {
		endLine--
	}
	c.add(startLine, endLine, kind)
}

func onlyClosersBefore(src []byte, offset int) bool {
	for i := offset - 1; i >= 0; i-- {
		switch src[i] {
		case ' ', '\t', '}', ']', ')':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

func onlyIndentBefore(src []byte, offset int) bool {
	for i := offset - 1; i >= 0; i-- {
		switch src[i] {
		case ' ', '\t':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

// add records a range between one-based HCL lines.
func (c *collector) add(startLine, endLine int, kind string) {
	if endLine <= startLine {
		return
	}
	key := [2]uint32{uint32(startLine - 1), uint32(endLine - 1)}
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.ranges = append(c.ranges, lsp.FoldingRange{
		StartLine: key[0],
		EndLine:   key[1],
		Kind:      kind,
	})
}

// comments folds runs of comments on consecutive lines,
// and multi-line block comments.
func (c *collector) comments(filename string) {
	tokens, _ := hclsyntax.LexConfig(c.src, filename, hcl.InitialPos)
	runStart, runEnd := -1, -1
	flush := func() {
		if runStart >= 0 {
			c.add(runStart, runEnd, string(lsp.Comment))
		}
		runStart, runEnd = -1, -1
	}
	for _, tok := range tokens {
		if tok.Type != hclsyntax.TokenComment {
			if tok.Type == hclsyntax.TokenNewline && runStart >= 0 && tok.Range.Start.Line <= runEnd {
				continue
			}
			flush()
			continue
		}
		startLine := tok.Range.Start.Line
		endLine := tok.Range.End.Line
		// a line comment's token includes its newline
		if bytes.HasSuffix(tok.Bytes, []byte("\n")) {
			endLine = tok.Range.Start.Line
		}
		if runStart >= 0 && startLine == runEnd+1 && onlyIndentBefore(c.src, tok.Range.Start.Byte) {
			runEnd = endLine
			continue
		}
		flush()
		if !onlyIndentBefore(c.src, tok.Range.Start.Byte) {
			// a trailing comment after code does not start a run
			continue
		}
		runStart, runEnd = startLine, endLine
	}
	flush()
}
