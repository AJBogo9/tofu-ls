// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// sourceFile is a parsed file of a module.
type sourceFile struct {
	path string
	src  []byte
	body *hclsyntax.Body
}

// moduleSources returns the parsed native syntax files of the document's
// module, the document first. ok is false when any of them has syntax
// errors: what the errors hide could still refer to what an action
// removes.
func moduleSources(env Env, doc Document) ([]sourceFile, bool) {
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return nil, false
	}
	files := []sourceFile{{path: doc.Path, src: doc.Text, body: body}}
	for _, path := range env.moduleFiles(doc.dir()) {
		if path == doc.Path {
			continue
		}
		src, ok := env.readFile(path)
		if !ok {
			continue
		}
		body, ok := env.parse(path, src)
		if !ok {
			return nil, false
		}
		files = append(files, sourceFile{path: path, src: src, body: body})
	}
	return files, true
}

// touches tells whether two ranges of one file overlap or meet.
func touches(a, b hcl.Range) bool {
	return a.Start.Byte <= b.End.Byte && b.Start.Byte <= a.End.Byte
}

// deletions collects deletions per file and merges those that overlap,
// such as two neighbouring elements which both take the blank line
// between them.
type deletions struct {
	order []string
	src   map[string][]byte
	spans map[string][][2]int
}

func newDeletions() *deletions {
	return &deletions{src: map[string][]byte{}, spans: map[string][][2]int{}}
}

func (d *deletions) add(file string, src []byte, start, end int) {
	if _, ok := d.src[file]; !ok {
		d.order = append(d.order, file)
		d.src[file] = src
	}
	e := deleteElement(file, src, start, end)
	d.spans[file] = append(d.spans[file], [2]int{e.Range.Start.Byte, e.Range.End.Byte})
}

func (d *deletions) has(file string) bool {
	return len(d.spans[file]) > 0
}

func (d *deletions) edits() []Edit {
	var edits []Edit
	for _, file := range d.order {
		spans := d.spans[file]
		sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
		merged := make([][2]int, 0, len(spans))
		for _, s := range spans {
			if n := len(merged); n > 0 && s[0] <= merged[n-1][1] {
				if s[1] > merged[n-1][1] {
					merged[n-1][1] = s[1]
				}
				continue
			}
			merged = append(merged, s)
		}
		edits = append(edits, realigned(file, d.src[file], merged)...)
	}
	return edits
}

// realigned returns the edits which delete the spans of src, and realign
// the equals signs of the arguments around them (a shorter longest name
// narrows the column) where their top-level block, or for top-level
// arguments the file, was formatted before.
func realigned(file string, src []byte, spans [][2]int) []Edit {
	raw := make([]Edit, 0, len(spans))
	for _, s := range spans {
		raw = append(raw, replace(file, src, s[0], s[1], ""))
	}
	return laidOut(file, src, raw)
}

// alsoIn lists the files of locations other than the document's module
// files, relative to dir, for a title.
func alsoIn(dir string, locs []Location) string {
	seen := map[string]bool{}
	names := make([]string, 0)
	for _, loc := range locs {
		rel, err := filepath.Rel(dir, loc.File)
		if err != nil {
			rel = loc.File
		}
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			names = append(names, rel)
		}
	}
	sort.Strings(names)
	if len(names) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
	}
	return strings.Join(names, ", ")
}

// removeUnusedVariable deletes the variable's declaration (and its
// override declarations) and every place outside the module which sets
// it: var files, test files and module call arguments, which would
// otherwise set a variable that no longer exists.
func removeUnusedVariable(env Env, doc Document, diag Diagnostic) []Action {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	name := diag.dataString("name")
	if name == "" {
		for _, block := range files[0].body.Blocks {
			if block.Type == "variable" && len(block.Labels) == 1 && touches(block.LabelRanges[0], diag.Range) {
				name = block.Labels[0]
			}
		}
	}
	if name == "" {
		return nil
	}

	dels := newDeletions()
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type == "variable" && len(block.Labels) == 1 && block.Labels[0] == name {
				dels.add(f.path, f.src, block.Range().Start.Byte, block.Range().End.Byte)
			}
		}
	}
	if !dels.has(doc.Path) {
		return nil
	}

	var settings []Location
	if env.VariableSettings != nil {
		if settings, ok = env.VariableSettings(doc.dir(), name); !ok {
			return nil
		}
	}
	for _, loc := range settings {
		src, ok := env.readFile(loc.File)
		if !ok || loc.Range.End.Byte > len(src) {
			return nil
		}
		dels.add(loc.File, src, loc.Range.Start.Byte, loc.Range.End.Byte)
	}

	title := fmt.Sprintf("Remove unused variable %q", name)
	if len(settings) > 0 {
		title += fmt.Sprintf(" and where it is set (%s)", alsoIn(doc.dir(), settings))
	}
	return []Action{{Title: title, Preferred: true, Edits: dels.edits()}}
}

// removeUnusedLocal deletes the local value, and its locals block when
// nothing else is left in it.
func removeUnusedLocal(env Env, doc Document, diag Diagnostic) []Action {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	name := diag.dataString("name")
	if name == "" {
		for _, block := range files[0].body.Blocks {
			if block.Type != "locals" {
				continue
			}
			for _, attr := range block.Body.Attributes {
				if touches(attr.NameRange, diag.Range) {
					name = attr.Name
				}
			}
		}
	}
	if name == "" {
		return nil
	}

	dels := newDeletions()
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type != "locals" {
				continue
			}
			attr, ok := block.Body.Attributes[name]
			if !ok {
				continue
			}
			if len(block.Body.Attributes) == 1 && len(block.Body.Blocks) == 0 {
				dels.add(f.path, f.src, block.Range().Start.Byte, block.Range().End.Byte)
				continue
			}
			dels.add(f.path, f.src, attr.SrcRange.Start.Byte, attr.SrcRange.End.Byte)
		}
	}
	if !dels.has(doc.Path) {
		return nil
	}
	return []Action{{Title: fmt.Sprintf("Remove unused local %q", name), Preferred: true, Edits: dels.edits()}}
}

// removeUnusedDataSource deletes the data block, top-level or scoped to a
// check block.
func removeUnusedDataSource(env Env, doc Document, diag Diagnostic) []Action {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	var typ, name string
	if parts := strings.Split(diag.dataString("address"), "."); len(parts) == 3 && parts[0] == "data" {
		typ, name = parts[1], parts[2]
	} else {
		for _, block := range dataBlocks(files[0].body) {
			if len(block.Labels) == 2 && (touches(block.LabelRanges[0], diag.Range) || touches(block.LabelRanges[1], diag.Range)) {
				typ, name = block.Labels[0], block.Labels[1]
			}
		}
	}
	if typ == "" {
		return nil
	}

	dels := newDeletions()
	for _, f := range files {
		for _, block := range dataBlocks(f.body) {
			if len(block.Labels) == 2 && block.Labels[0] == typ && block.Labels[1] == name {
				dels.add(f.path, f.src, block.Range().Start.Byte, block.Range().End.Byte)
			}
		}
	}
	if !dels.has(doc.Path) {
		return nil
	}
	return []Action{{Title: fmt.Sprintf("Remove unused data source data.%s.%s", typ, name), Preferred: true, Edits: dels.edits()}}
}

// dataBlocks returns the data blocks of a file: top-level ones and those
// scoped to check blocks.
func dataBlocks(body *hclsyntax.Body) []*hclsyntax.Block {
	var blocks []*hclsyntax.Block
	for _, block := range body.Blocks {
		switch block.Type {
		case "data":
			blocks = append(blocks, block)
		case "check":
			for _, nested := range block.Body.Blocks {
				if nested.Type == "data" {
					blocks = append(blocks, nested)
				}
			}
		}
	}
	return blocks
}
