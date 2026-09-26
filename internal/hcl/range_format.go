// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"bytes"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// FormatLines returns before with the top-level blocks and attributes
// that the lines startLine to endLine (0-based, inclusive) touch replaced
// by their text in after, which is before formatted as a whole. Blocks are
// taken whole, lines included, so the result for them is what formatting
// the document gives.
//
// It returns before unchanged when the lines touch no top-level item, and
// false when before or after does not parse or their top-level items do
// not correspond.
func FormatLines(filename string, before, after []byte, startLine, endLine int) ([]byte, bool) {
	beforeItems, ok := topLevelItems(filename, before)
	if !ok {
		return nil, false
	}
	afterItems, ok := topLevelItems(filename, after)
	if !ok || len(afterItems) != len(beforeItems) {
		return nil, false
	}

	first, last := -1, -1
	for i, rng := range beforeItems {
		// HCL lines are 1-based
		if rng.End.Line-1 < startLine || rng.Start.Line-1 > endLine {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return before, true
	}

	bStart, bEnd := lineSpan(before, beforeItems[first].Start.Byte, beforeItems[last].End.Byte)
	aStart, aEnd := lineSpan(after, afterItems[first].Start.Byte, afterItems[last].End.Byte)

	partial := make([]byte, 0, len(before))
	partial = append(partial, before[:bStart]...)
	partial = append(partial, after[aStart:aEnd]...)
	partial = append(partial, before[bEnd:]...)
	return partial, true
}

// topLevelItems returns the ranges of the blocks and attributes of a
// file's body, in source order.
func topLevelItems(filename string, src []byte) ([]hcl.Range, bool) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, false
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, false
	}
	items := make([]hcl.Range, 0, len(body.Blocks)+len(body.Attributes))
	for _, b := range body.Blocks {
		items = append(items, b.Range())
	}
	for _, a := range body.Attributes {
		items = append(items, a.Range())
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Start.Byte < items[j].Start.Byte
	})
	return items, true
}

// lineSpan widens the bytes from start to end to whole lines, including
// the final newline.
func lineSpan(src []byte, start, end int) (int, int) {
	start = bytes.LastIndexByte(src[:start], '\n') + 1
	if i := bytes.IndexByte(src[end:], '\n'); i >= 0 {
		end += i + 1
	} else {
		end = len(src)
	}
	return start, end
}
