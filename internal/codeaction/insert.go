// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// indentUnit is the indentation of one nesting level, as tofu fmt writes
// it.
const indentUnit = "  "

// indentLines indents every non-empty line of text (lines separated by
// "\n") with indent.
func indentLines(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = indent + line
		}
	}
	return strings.Join(lines, "\n")
}

// insertInBody returns the edits which add attrs (after the body's last
// attribute) and blocks (at the end of the body) to the body of block.
// Each item is text without indentation, possibly several lines; it is
// indented like the rest of the body. A body written on one line is
// spread over several.
func insertInBody(file string, src []byte, block *hclsyntax.Block, attrs, blocks []string) []Edit {
	if len(attrs) == 0 && len(blocks) == 0 {
		return nil
	}
	open := block.OpenBraceRange.End.Byte
	close := block.CloseBraceRange.Start.Byte
	outer := indentAt(src, block.Range().Start.Byte)

	inner := outer + indentUnit
	if first := firstItemStart(block.Body); first >= 0 && lineStart(src, first) > lineStart(src, open) {
		inner = indentAt(src, first)
	}

	if lineStart(src, close) == lineStart(src, open-1) {
		// { } or { a = 1 } on one line
		items := make([]string, 0)
		if existing := strings.TrimSpace(string(src[open:close])); existing != "" {
			items = append(items, existing)
		}
		items = append(items, attrs...)
		items = append(items, blocks...)
		text := "\n" + indentLines(strings.Join(items, "\n"), inner) + "\n" + outer
		return []Edit{replace(file, src, open, close, withNewlines(text, src))}
	}

	var edits []Edit
	if len(attrs) > 0 {
		at := nextLine(src, open)
		if last := lastAttributeEnd(block.Body); last >= 0 {
			at = nextLine(src, last)
		}
		text := indentLines(strings.Join(attrs, "\n"), inner) + "\n"
		edits = append(edits, replace(file, src, at, at, withNewlines(text, src)))
	}
	if len(blocks) > 0 {
		at := lineStart(src, close)
		text := indentLines(strings.Join(blocks, "\n\n"), inner) + "\n"
		if prev := prevLine(src, at); !blank(prev) && !opens(prev) {
			text = "\n" + text
		}
		if !blank(src[at:close]) {
			// the closing brace follows other content on its line
			at = close
			text = "\n" + text + outer
		}
		edits = append(edits, replace(file, src, at, at, withNewlines(text, src)))
	}
	return edits
}

// firstItemStart is the offset of the first attribute or block of a
// body, or -1 for an empty body.
func firstItemStart(body *hclsyntax.Body) int {
	first := -1
	for _, attr := range body.Attributes {
		if b := attr.SrcRange.Start.Byte; first < 0 || b < first {
			first = b
		}
	}
	for _, block := range body.Blocks {
		if b := block.Range().Start.Byte; first < 0 || b < first {
			first = b
		}
	}
	return first
}

// lastAttributeEnd is the offset of the end of the last attribute of a
// body, or -1 when it has none.
func lastAttributeEnd(body *hclsyntax.Body) int {
	last := -1
	for _, attr := range body.Attributes {
		if b := attr.SrcRange.End.Byte; b > last {
			last = b
		}
	}
	return last
}
