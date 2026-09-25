// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package selection computes the ranges for expanding a selection
// (smart select) from the HCL syntax tree.
package selection

import (
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Ranges returns the syntax ranges which contain pos, innermost first:
// a reference step, the reference, the enclosing expressions, the
// attribute, the block body and the block, up to the whole file.
func Ranges(src []byte, filename string, pos hcl.Pos) []hcl.Range {
	f, _ := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if f == nil {
		return nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}

	ranges := make([]hcl.Range, 0)
	add := func(rng hcl.Range) {
		if rng.Start.Byte <= pos.Byte && pos.Byte <= rng.End.Byte && rng.End.Byte > rng.Start.Byte {
			ranges = append(ranges, rng)
		}
	}

	hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		switch n := node.(type) {
		case *hclsyntax.Body:
			// the file body's range is the file; a block body's range
			// is covered by the block, and its inside is added below
			add(n.SrcRange)
		case *hclsyntax.Block:
			add(n.Range())
			// the inside of the braces
			inside := hcl.Range{Filename: filename, Start: n.OpenBraceRange.End, End: n.CloseBraceRange.Start}
			add(inside)
			for _, rng := range n.LabelRanges {
				add(rng)
			}
			add(n.TypeRange)
		case *hclsyntax.Attribute:
			add(n.SrcRange)
			add(n.NameRange)
		case *hclsyntax.ScopeTraversalExpr:
			add(n.SrcRange)
			for _, step := range n.Traversal {
				rng := step.SourceRange()
				if attr, ok := step.(hcl.TraverseAttr); ok && rng.End.Byte-rng.Start.Byte == len(attr.Name)+1 {
					// the name without its dot
					rng.Start = hcl.Pos{Line: rng.Start.Line, Column: rng.Start.Column + 1, Byte: rng.Start.Byte + 1}
				}
				add(rng)
			}
		case hclsyntax.Expression:
			add(n.Range())
		}
		return nil
	})

	// innermost first; ranges that are not nested in the next one would
	// break the chain, so keep only a strictly growing chain of containers
	sort.SliceStable(ranges, func(i, j int) bool {
		return size(ranges[i]) < size(ranges[j])
	})
	chain := make([]hcl.Range, 0, len(ranges))
	for _, rng := range ranges {
		if len(chain) > 0 {
			last := chain[len(chain)-1]
			if size(rng) == size(last) && rng.Start.Byte == last.Start.Byte {
				continue
			}
			if rng.Start.Byte > last.Start.Byte || rng.End.Byte < last.End.Byte {
				continue
			}
		}
		chain = append(chain, rng)
	}
	return chain
}

func size(rng hcl.Range) int {
	return rng.End.Byte - rng.Start.Byte
}
