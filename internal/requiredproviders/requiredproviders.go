// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package requiredproviders reads and edits the required_providers
// entries of a module's terraform blocks, and finds the provider local
// names that the module's resources use.
package requiredproviders

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Entry is one entry of a required_providers block, such as
// `aws = { source = "hashicorp/aws" }`.
type Entry struct {
	// Name is the provider's local name, the entry's key.
	Name     string
	Filename string
	Attr     *hclsyntax.Attribute
	// Source is the static value of the entry's source argument, empty
	// when there is none.
	Source string
}

// Entries returns the required_providers entries of every terraform
// block in files, keyed by local name. When a name is declared twice, the
// first file in name order wins.
func Entries(files map[string]*hcl.File) map[string]Entry {
	entries := make(map[string]Entry)
	for _, name := range sortedNames(files) {
		body, ok := files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, tf := range body.Blocks {
			if tf.Type != "terraform" {
				continue
			}
			for _, rp := range tf.Body.Blocks {
				if rp.Type != "required_providers" {
					continue
				}
				for _, attr := range sortedAttributes(rp.Body) {
					if _, ok := entries[attr.Name]; ok {
						continue
					}
					entries[attr.Name] = newEntry(name, attr)
				}
			}
		}
	}
	return entries
}

// EntryAtPos returns the required_providers entry whose value holds pos.
func EntryAtPos(file *hcl.File, filename string, pos hcl.Pos) (Entry, bool) {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return Entry{}, false
	}
	for _, tf := range body.Blocks {
		if tf.Type != "terraform" || !tf.Range().ContainsPos(pos) {
			continue
		}
		for _, rp := range tf.Body.Blocks {
			if rp.Type != "required_providers" || !rp.Body.Range().ContainsPos(pos) {
				continue
			}
			for _, attr := range rp.Body.Attributes {
				rng := attr.Expr.Range()
				if rng.ContainsPos(pos) || rng.End.Byte == pos.Byte {
					return newEntry(filename, attr), true
				}
			}
		}
	}
	return Entry{}, false
}

// NewEntryAtPos tells whether pos is where a new required_providers entry
// starts: in the body of a required_providers block of a terraform block,
// on a line with nothing else than the name typed so far. It reads the
// tokens rather than the syntax tree, since a half-typed name makes the
// block invalid.
func NewEntryAtPos(src []byte, filename string, pos hcl.Pos) bool {
	if pos.Byte > len(src) {
		return false
	}
	tokens, _ := hclsyntax.LexConfig(src, filename, hcl.InitialPos)
	var open []string
	var prev hclsyntax.Token
	for _, t := range tokens {
		if t.Range.Start.Byte >= pos.Byte {
			break
		}
		switch t.Type {
		case hclsyntax.TokenOBrace:
			name := ""
			if prev.Type == hclsyntax.TokenIdent {
				name = string(prev.Bytes)
			}
			open = append(open, name)
		case hclsyntax.TokenCBrace:
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		}
		if t.Type != hclsyntax.TokenNewline {
			prev = t
		}
	}
	if len(open) != 2 || open[0] != "terraform" || open[1] != "required_providers" {
		return false
	}

	lineStart := bytes.LastIndexByte(src[:pos.Byte], '\n') + 1
	lineEnd := len(src)
	if i := bytes.IndexByte(src[pos.Byte:], '\n'); i >= 0 {
		lineEnd = pos.Byte + i
	}
	start, end := IdentAround(src, pos.Byte)
	return strings.TrimSpace(string(src[lineStart:start])) == "" &&
		strings.TrimSpace(string(src[end:lineEnd])) == ""
}

// IdentAround returns the byte range of the identifier characters around
// offset in src.
func IdentAround(src []byte, offset int) (int, int) {
	isIdent := func(b byte) bool {
		return b == '_' || b == '-' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	start, end := offset, offset
	for start > 0 && isIdent(src[start-1]) {
		start--
	}
	for end < len(src) && isIdent(src[end]) {
		end++
	}
	return start, end
}

func newEntry(filename string, attr *hclsyntax.Attribute) Entry {
	e := Entry{
		Name:     attr.Name,
		Filename: filename,
		Attr:     attr,
	}
	obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return e
	}
	for _, item := range obj.Items {
		key, ok := keyName(item.KeyExpr)
		if !ok || key != "source" {
			continue
		}
		v, diags := item.ValueExpr.Value(nil)
		if !diags.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
			e.Source = v.AsString()
		}
	}
	return e
}

func keyName(expr hclsyntax.Expression) (string, bool) {
	if k, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		expr = k.Wrapped
	}
	if t, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok && len(t.Traversal) == 1 {
		return t.Traversal.RootName(), true
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.Type() != cty.String || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	return v.AsString(), true
}

// UsedLocalNames returns the provider local names which the module's
// resources, data sources, ephemeral resources and provider blocks use,
// sorted. The built-in terraform provider is left out.
func UsedLocalNames(files map[string]*hcl.File) []string {
	used := make(map[string]struct{})
	var visit func(blocks hclsyntax.Blocks)
	visit = func(blocks hclsyntax.Blocks) {
		for _, block := range blocks {
			switch block.Type {
			case "resource", "data", "ephemeral":
				if len(block.Labels) == 0 {
					continue
				}
				if name, ok := ResourceLocalName(block.Labels[0], block.Body); ok {
					used[name] = struct{}{}
				}
			case "provider":
				if len(block.Labels) > 0 {
					used[block.Labels[0]] = struct{}{}
				}
			case "check":
				visit(block.Body.Blocks)
			}
		}
	}
	for _, f := range files {
		if body, ok := f.Body.(*hclsyntax.Body); ok {
			visit(body.Blocks)
		}
	}
	delete(used, "terraform")
	delete(used, "")

	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResourceLocalName returns the local name of the provider which a
// resource or data source of typeName uses: the one its provider argument
// names, else the type's prefix up to the first underscore, as OpenTofu
// implies it.
func ResourceLocalName(typeName string, body *hclsyntax.Body) (string, bool) {
	if body != nil {
		if attr, ok := body.Attributes["provider"]; ok {
			if t, ok := attr.Expr.(*hclsyntax.ScopeTraversalExpr); ok {
				return t.Traversal.RootName(), true
			}
			return "", false
		}
	}
	name, _, _ := strings.Cut(typeName, "_")
	return name, name != ""
}

// Edit is a text edit of one file.
type Edit struct {
	Filename string
	// Range is empty for an insertion.
	Range   hcl.Range
	NewText string
}

// AddEntryEdit returns the edit which declares a required_providers entry
// for name with the given source:
//
//   - into a required_providers block, in the first file which has one;
//   - else as a new required_providers block, in the first terraform block;
//   - else as a new terraform block at the end of versions.tf when there
//     is such a file, or at the top of currentFile.
//
// The text is formatted as tofu fmt formats it. JSON files are left alone.
func AddEntryEdit(files map[string]*hcl.File, currentFile, name, source string) (Edit, bool) {
	var firstTerraform *hclsyntax.Block
	var firstTerraformFile string
	for _, filename := range sortedNames(files) {
		body, ok := files[filename].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, tf := range body.Blocks {
			if tf.Type != "terraform" {
				continue
			}
			if firstTerraform == nil {
				firstTerraform, firstTerraformFile = tf, filename
			}
			for _, rp := range tf.Body.Blocks {
				if rp.Type == "required_providers" {
					src := files[filename].Bytes
					return appendToBlock(filename, src, rp, entryLines(name, source)), true
				}
			}
		}
	}

	if firstTerraform != nil {
		src := files[firstTerraformFile].Bytes
		lines := append([]string{"required_providers {"}, indentLines(entryLines(name, source))...)
		lines = append(lines, "}")
		return appendToBlock(firstTerraformFile, src, firstTerraform, lines), true
	}

	block := terraformBlockText(name, source)
	if f, ok := files["versions.tf"]; ok {
		return appendToFile("versions.tf", f.Bytes, block), true
	}
	f, ok := files[currentFile]
	if !ok || strings.HasSuffix(currentFile, ".json") {
		return Edit{}, false
	}
	return prependToFile(currentFile, f, block), true
}

func entryLines(name, source string) []string {
	return []string{
		fmt.Sprintf("%s = {", name),
		fmt.Sprintf("  source = %q", source),
		"}",
	}
}

func indentLines(lines []string) []string {
	indented := make([]string, len(lines))
	for i, l := range lines {
		indented[i] = "  " + l
	}
	return indented
}

func terraformBlockText(name, source string) string {
	lines := []string{"terraform {", "  required_providers {"}
	lines = append(lines, indentLines(indentLines(entryLines(name, source)))...)
	lines = append(lines, "  }", "}")
	return strings.Join(lines, "\n") + "\n"
}

// appendToBlock inserts lines, indented one level below block, as the last
// items of block's body.
func appendToBlock(filename string, src []byte, block *hclsyntax.Block, lines []string) Edit {
	indent := lineIndent(src, block.TypeRange.Start.Byte) + "  "
	var text strings.Builder
	for _, l := range lines {
		text.WriteString(indent + l + "\n")
	}

	closeRng := block.CloseBraceRange
	if block.OpenBraceRange.Start.Line == closeRng.Start.Line {
		// `required_providers {}`, or a single-line block with one
		// argument: put its content on lines of its own
		inner := strings.TrimSpace(string(src[block.OpenBraceRange.End.Byte:closeRng.Start.Byte]))
		if inner != "" {
			inner = indent + inner + "\n"
		}
		return Edit{
			Filename: filename,
			Range: hcl.Range{
				Filename: filename,
				Start:    block.OpenBraceRange.End,
				End:      closeRng.Start,
			},
			NewText: "\n" + inner + text.String() + lineIndent(src, block.TypeRange.Start.Byte),
		}
	}

	// insert at the start of the closing brace's line, when the brace is
	// the first thing on it
	lineStart := bytes.LastIndexByte(src[:closeRng.Start.Byte], '\n') + 1
	if strings.TrimSpace(string(src[lineStart:closeRng.Start.Byte])) == "" {
		pos := hcl.Pos{Line: closeRng.Start.Line, Column: 1, Byte: lineStart}
		return Edit{
			Filename: filename,
			Range:    hcl.Range{Filename: filename, Start: pos, End: pos},
			NewText:  text.String(),
		}
	}
	// `x = 1 }`: break the line before the brace, dropping the spaces
	// before it
	start := closeRng.Start
	for start.Byte > lineStart && (src[start.Byte-1] == ' ' || src[start.Byte-1] == '\t') {
		start.Byte--
		start.Column--
	}
	return Edit{
		Filename: filename,
		Range:    hcl.Range{Filename: filename, Start: start, End: closeRng.Start},
		NewText:  "\n" + text.String() + lineIndent(src, block.TypeRange.Start.Byte),
	}
}

// lineIndent returns the whitespace which starts the line of offset.
func lineIndent(src []byte, offset int) string {
	start := bytes.LastIndexByte(src[:offset], '\n') + 1
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

func appendToFile(filename string, src []byte, text string) Edit {
	end := endPos(src)
	prefix := ""
	trimmed := bytes.TrimRight(src, " \t\r\n")
	if len(trimmed) > 0 {
		prefix = "\n"
		if !bytes.HasSuffix(src, []byte("\n")) {
			prefix = "\n\n"
		}
	}
	return Edit{
		Filename: filename,
		Range:    hcl.Range{Filename: filename, Start: end, End: end},
		NewText:  prefix + text,
	}
}

// prependToFile inserts text above the first block of the file, and above
// the comments attached to it, but below a leading comment which a blank
// line separates, such as a license header.
func prependToFile(filename string, f *hcl.File, text string) Edit {
	pos := hcl.InitialPos
	body, ok := f.Body.(*hclsyntax.Body)
	if ok && len(body.Blocks)+len(body.Attributes) > 0 {
		first := firstItemStart(body)
		lines := bytes.Split(f.Bytes[:bytes.LastIndexByte(f.Bytes[:first.Byte], '\n')+1], []byte("\n"))
		// lines ends with the empty text before the first block's line
		lines = lines[:len(lines)-1]
		attached := 0
		for i := len(lines) - 1; i >= 0; i-- {
			l := bytes.TrimSpace(lines[i])
			if len(l) == 0 {
				break
			}
			if !bytes.HasPrefix(l, []byte("#")) && !bytes.HasPrefix(l, []byte("//")) {
				break
			}
			attached++
		}
		if attached < len(lines) {
			line := len(lines) - attached
			offset := 0
			for _, l := range lines[:line] {
				offset += len(l) + 1
			}
			pos = hcl.Pos{Line: line + 1, Column: 1, Byte: offset}
		} else {
			pos = hcl.InitialPos
		}
	}
	return Edit{
		Filename: filename,
		Range:    hcl.Range{Filename: filename, Start: pos, End: pos},
		NewText:  text + "\n",
	}
}

func firstItemStart(body *hclsyntax.Body) hcl.Pos {
	var first *hcl.Pos
	for _, b := range body.Blocks {
		p := b.Range().Start
		if first == nil || p.Byte < first.Byte {
			first = &p
		}
	}
	for _, a := range body.Attributes {
		p := a.Range().Start
		if first == nil || p.Byte < first.Byte {
			first = &p
		}
	}
	return *first
}

func endPos(src []byte) hcl.Pos {
	line := bytes.Count(src, []byte("\n")) + 1
	lastNL := bytes.LastIndexByte(src, '\n')
	return hcl.Pos{Line: line, Column: len(src) - lastNL, Byte: len(src)}
}

func sortedNames(files map[string]*hcl.File) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedAttributes(body *hclsyntax.Body) []*hclsyntax.Attribute {
	attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
	for _, attr := range body.Attributes {
		attrs = append(attrs, attr)
	}
	sort.Slice(attrs, func(i, j int) bool {
		return attrs[i].SrcRange.Start.Byte < attrs[j].SrcRange.Start.Byte
	})
	return attrs
}
