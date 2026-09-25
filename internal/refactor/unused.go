// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// UnusedSymbol is a variable or local value which nothing
// in its module references.
type UnusedSymbol struct {
	Kind     SymbolKind
	Name     string
	Filename string
	// NameRange is the variable's label (with quotes) or the local's name.
	NameRange hcl.Range
}

// UnusedSymbols finds the variables and locals of a module which no
// expression of the module references. It looks at the syntax of every
// file, not at decoded references, so references the schema does not
// decode (validation conditions, blocks without a schema) still count.
//
// A reference to a variable from its own validation block does not count.
//
// It returns nothing (rather than guessing) when any file is JSON
// or failed to parse, since references in it could be missed.
func UnusedSymbols(files map[string]*hcl.File) []UnusedSymbol {
	declared := make([]UnusedSymbol, 0)
	used := map[string]bool{}

	filenames := make([]string, 0, len(files))
	for name := range files {
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)

	for _, name := range filenames {
		f := files[name]
		if f == nil || strings.HasSuffix(name, ".json") {
			return nil
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			return nil
		}

		for _, block := range body.Blocks {
			self := ""
			switch block.Type {
			case "variable":
				if len(block.Labels) == 1 && len(block.LabelRanges) == 1 {
					// the whole label, quotes included, so that the faded
					// name stands out
					declared = append(declared, UnusedSymbol{Kind: KindVariable, Name: block.Labels[0], Filename: name, NameRange: block.LabelRanges[0]})
					self = "var." + block.Labels[0]
				}
			case "locals":
				for _, attr := range block.Body.Attributes {
					declared = append(declared, UnusedSymbol{Kind: KindLocal, Name: attr.Name, Filename: name, NameRange: attr.NameRange})
				}
			}

			wrapper := &hclsyntax.Body{Blocks: hclsyntax.Blocks{block}}
			_ = walkBodyTraversals(wrapper, "", false, func(tr hcl.Traversal, _ bool) error {
				key, ok := symbolKey(tr)
				if ok && key != self {
					used[key] = true
				}
				return nil
			})
		}
		// top-level attributes do not exist in module files, but be safe
		_ = walkBodyTraversals(&hclsyntax.Body{Attributes: body.Attributes}, "", false, func(tr hcl.Traversal, _ bool) error {
			if key, ok := symbolKey(tr); ok {
				used[key] = true
			}
			return nil
		})
	}

	unused := make([]UnusedSymbol, 0)
	for _, sym := range declared {
		prefix := "var."
		if sym.Kind == KindLocal {
			prefix = "local."
		}
		if !used[prefix+sym.Name] {
			unused = append(unused, sym)
		}
	}
	return unused
}

// symbolKey returns var.<name> or local.<name> for traversals
// of variables and locals.
func symbolKey(tr hcl.Traversal) (string, bool) {
	if len(tr) < 2 {
		return "", false
	}
	root, ok := tr[0].(hcl.TraverseRoot)
	if !ok || (root.Name != "var" && root.Name != "local") {
		return "", false
	}
	attr, ok := tr[1].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	return root.Name + "." + attr.Name, true
}
