// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"bytes"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// parse parses a native syntax file. Actions never edit a file with
// syntax errors, since what the error hides could be lost.
func parse(filename string, src []byte) (*hclsyntax.Body, bool) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() || f == nil {
		return nil, false
	}
	body, ok := f.Body.(*hclsyntax.Body)
	return body, ok
}

// posAt is the position of the byte offset b in src.
func posAt(src []byte, b int) hcl.Pos {
	start := lineStart(src, b)
	return hcl.Pos{
		Line:   bytes.Count(src[:b], []byte("\n")) + 1,
		Column: b - start + 1,
		Byte:   b,
	}
}

func rangeAt(file string, src []byte, start, end int) hcl.Range {
	return hcl.Range{Filename: file, Start: posAt(src, start), End: posAt(src, end)}
}

// replace is the edit which replaces the bytes start to end of src (the
// text of file) with text.
func replace(file string, src []byte, start, end int, text string) Edit {
	return Edit{File: file, Range: rangeAt(file, src, start, end), NewText: text}
}

// lineStart is the offset of the start of the line holding b.
func lineStart(src []byte, b int) int {
	return bytes.LastIndexByte(src[:b], '\n') + 1
}

// lineEnd is the offset of the newline ending the line holding b, or the
// end of src.
func lineEnd(src []byte, b int) int {
	i := bytes.IndexByte(src[b:], '\n')
	if i < 0 {
		return len(src)
	}
	return b + i
}

// nextLine is the offset of the start of the line after the one
// holding b, or the end of src.
func nextLine(src []byte, b int) int {
	e := lineEnd(src, b)
	if e < len(src) {
		return e + 1
	}
	return e
}

// prevLine returns the line before the one starting at s.
func prevLine(src []byte, s int) []byte {
	if s == 0 {
		return nil
	}
	return src[lineStart(src, s-1) : s-1]
}

func blank(line []byte) bool {
	return len(bytes.TrimSpace(line)) == 0
}

func isComment(line []byte) bool {
	t := bytes.TrimSpace(line)
	return len(t) > 0 && (t[0] == '#' || bytes.HasPrefix(t, []byte("//")))
}

func opens(line []byte) bool {
	return bytes.HasSuffix(bytes.TrimSpace(line), []byte("{"))
}

func closes(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("}"))
}

// indentAt is the indentation of the line holding b.
func indentAt(src []byte, b int) string {
	s := lineStart(src, b)
	i := s
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[s:i])
}

// aloneOnLines tells whether the element from start to end has only
// whitespace before it on its first line and only whitespace or a
// comment after it on its last line.
func aloneOnLines(src []byte, start, end int) bool {
	if !blank(src[lineStart(src, start):start]) {
		return false
	}
	after := src[end:lineEnd(src, end)]
	return blank(after) || isComment(after)
}

// deleteElement is the edit which deletes an element (a block or an
// attribute) from start to end of src, the text of file. An element
// alone on its lines is deleted with its lines, with the comment lines
// right above it when they form a paragraph with it, and with one of two
// blank lines that would otherwise meet.
func deleteElement(file string, src []byte, start, end int) Edit {
	if !aloneOnLines(src, start, end) {
		return replace(file, src, start, end, "")
	}
	s := lineStart(src, start)
	e := nextLine(src, end)

	// comments right above describe the element when nothing else
	// shares their paragraph
	c := s
	for c > 0 && isComment(prevLine(src, c)) {
		c = lineStart(src, c-1)
	}
	if c < s {
		before := c == 0 || blank(prevLine(src, c)) || opens(prevLine(src, c))
		after := e >= len(src) || blank(src[e:lineEnd(src, e)]) || closes(src[e:lineEnd(src, e)])
		if before && after {
			s = c
		}
	}

	prevBlank := s > 0 && blank(prevLine(src, s))
	prevOpens := s == 0 || opens(prevLine(src, s))
	nextBlank := e < len(src) && blank(src[e:lineEnd(src, e)])
	nextCloses := e >= len(src) || closes(src[e:lineEnd(src, e)])
	switch {
	case (prevBlank || prevOpens) && nextBlank:
		e = nextLine(src, e)
	case prevBlank && nextCloses:
		s = lineStart(src, s-1)
	}
	return replace(file, src, s, e, "")
}

// newline is the line ending of src.
func newline(src []byte) string {
	if bytes.Contains(src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// withNewlines converts the line endings of text to those of src.
func withNewlines(text string, src []byte) string {
	if newline(src) == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// appendBlock is the edit which appends a top-level block (text ending
// with a newline) to the end of src, separated by one blank line.
func appendBlock(file string, src []byte, text string) Edit {
	trimmed := bytes.TrimRight(src, " \t")
	var prefix string
	switch {
	case len(bytes.TrimSpace(src)) == 0:
		prefix = ""
	case bytes.HasSuffix(trimmed, []byte("\n\n")) || bytes.HasSuffix(trimmed, []byte("\r\n\r\n")):
		prefix = ""
	case bytes.HasSuffix(trimmed, []byte("\n")):
		prefix = "\n"
	default:
		prefix = "\n\n"
	}
	return replace(file, src, len(src), len(src), withNewlines(prefix+text, src))
}

// applyEdits applies non-overlapping edits of one file to src.
func applyEdits(src []byte, edits []Edit) []byte {
	sorted := append([]Edit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Range.Start.Byte < sorted[j].Range.Start.Byte
	})
	var out bytes.Buffer
	last := 0
	for _, e := range sorted {
		out.Write(src[last:e.Range.Start.Byte])
		out.WriteString(e.NewText)
		last = e.Range.End.Byte
	}
	out.Write(src[last:])
	return out.Bytes()
}

// region is the lines of a file from start to end (offsets of line
// starts) which the formatter can lay out on their own: a top-level block
// or the whole file.
type region struct {
	start, end int
}

// topRegion is the region of the lines of a top-level block.
func topRegion(src []byte, top *hclsyntax.Block) region {
	return region{lineStart(src, top.Range().Start.Byte), nextLine(src, top.Range().End.Byte)}
}

// formattedRegion returns one edit which applies edits (all within r) and
// lays out the lines of r as the formatter would: it aligns the equals
// signs of inserted arguments with their neighbours, and realigns them
// when a deleted argument had the longest name. That happens only when r
// was formatted before, so nothing the user laid out differently changes;
// where only is not nil, the lines only[0] to only[1] of r (1-based,
// counted after the edits) are laid out anyway, since the edits wrote
// them all. Otherwise edits come back unchanged.
func formattedRegion(file string, src []byte, r region, edits []Edit, only *[2]int) []Edit {
	if len(edits) == 0 {
		return edits
	}
	old := src[r.start:r.end]
	wasFormatted := bytes.Equal(hclwrite.Format(old), old)
	if !wasFormatted && only == nil {
		return edits
	}
	shifted := make([]Edit, len(edits))
	for i, e := range edits {
		if e.Range.Start.Byte < r.start || e.Range.End.Byte > r.end {
			return edits
		}
		shifted[i] = e
		shifted[i].Range.Start.Byte -= r.start
		shifted[i].Range.End.Byte -= r.start
	}
	after := applyEdits(old, shifted)
	if _, ok := parse(file, after); !ok {
		return edits
	}
	// the formatter changes spaces only, never the lines
	out := hclwrite.Format(after)
	if !wasFormatted {
		out = spliceLines(after, out, only[0], only[1])
	}
	p, s, ok := diffBounds(old, out)
	if !ok {
		return nil
	}
	return []Edit{replace(file, src, r.start+p, r.end-s, string(out[p:len(out)-s]))}
}

// formattedBlock returns edits which insert the same content into block,
// a block of the top-level block top, as edits, laid out as the formatter
// would (see formattedRegion). A body written on one line, which the
// edits spread out, is laid out in any case.
func formattedBlock(file string, src []byte, top, block *hclsyntax.Block, edits []Edit) []Edit {
	var only *[2]int
	if lineStart(src, block.OpenBraceRange.Start.Byte) == lineStart(src, block.CloseBraceRange.Start.Byte) {
		added := 0
		for _, e := range edits {
			added += strings.Count(e.NewText, "\n") - bytes.Count(src[e.Range.Start.Byte:e.Range.End.Byte], []byte("\n"))
		}
		first := block.Range().Start.Line - top.Range().Start.Line + 1
		only = &[2]int{first, first + block.Range().End.Line - block.Range().Start.Line + added}
	}
	return formattedRegion(file, src, topRegion(src, top), edits, only)
}

// lines splits src into lines which keep their newline.
func lines(src []byte) [][]byte {
	return bytes.SplitAfter(src, []byte("\n"))
}

// spliceLines is a with its lines first to last (1-based, inclusive)
// taken from b, which has as many lines.
func spliceLines(a, b []byte, first, last int) []byte {
	la, lb := lines(a), lines(b)
	if len(la) != len(lb) || last > len(la) {
		return a
	}
	var out bytes.Buffer
	for i := range la {
		if i >= first-1 && i < last {
			out.Write(lb[i])
		} else {
			out.Write(la[i])
		}
	}
	return out.Bytes()
}

// diffBounds returns the length p of the whole lines a and b start with,
// and the length s of the whole lines of a they end with, so that
// a[p:len(a)-s] is to be replaced with b[p:len(b)-s]. ok is false when
// they are equal.
func diffBounds(a, b []byte) (p, s int, ok bool) {
	if bytes.Equal(a, b) {
		return 0, 0, false
	}
	for i := 0; i < len(a) && i < len(b) && a[i] == b[i]; i++ {
		if a[i] == '\n' {
			p = i + 1
		}
	}
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	// the shared end starts at the start of a line of a
	for s > 0 && len(a)-s > 0 && a[len(a)-s-1] != '\n' {
		s--
	}
	return p, s, true
}

// lineDiff is one edit which turns a into b, replacing whole lines
// between the lines they share at the start and at the end.
func lineDiff(file string, a, b []byte) (Edit, bool) {
	p, s, ok := diffBounds(a, b)
	if !ok {
		return Edit{}, false
	}
	return replace(file, a, p, len(a)-s, string(b[p:len(b)-s])), true
}
