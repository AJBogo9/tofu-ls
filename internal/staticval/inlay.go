// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// InlayHint is a value shown after a reference.
type InlayHint struct {
	// Pos is the end of the reference.
	Pos   hcl.Pos
	Label string
	// Ref is the reference as written, for the tooltip.
	Ref string
}

// DefaultInlayHintMaxLength is the default label length, in characters,
// excluding the leading "= ".
const DefaultInlayHintMaxLength = 40

// InlayHints returns a hint after every var and local reference in the
// file whose value is fully known and not sensitive. Only references that
// start inside rng are considered (a zero rng means the whole file).
//
// Some places get no hints at all: variable blocks, where they would only
// restate the variable's own value, and the places OpenTofu redacts, which
// are outputs marked sensitive and resource or data arguments that the
// provider schema marks sensitive.
func (ev *Evaluator) InlayHints(filename string, rng hcl.Range, maxLen int) []InlayHint {
	f, ok := ev.mod.Files[filename]
	if !ok {
		return nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	if maxLen <= 0 {
		maxLen = DefaultInlayHintMaxLength
	}

	var hints []InlayHint
	// A var or local reference has the same value wherever it is written,
	// so each distinct one is evaluated and rendered once: a file may
	// refer to one large value a thousand times.
	labels := make(map[string]string)
	visit := func(node hclsyntax.Node) {
		hclsyntax.VisitAll(node, func(n hclsyntax.Node) hcl.Diagnostics {
			if ev.cancelled() {
				return nil
			}
			expr, ok := n.(*hclsyntax.ScopeTraversalExpr)
			if !ok {
				return nil
			}
			exprRng := expr.Range()
			if rng.End.Byte > 0 && (exprRng.Start.Byte < rng.Start.Byte || exprRng.Start.Byte > rng.End.Byte) {
				return nil
			}
			switch expr.Traversal.RootName() {
			case "var", "local":
			default:
				return nil
			}
			if len(expr.Traversal) < 2 {
				return nil
			}
			key := traversalKey(expr.Traversal)
			label, seen := labels[key]
			if !seen {
				if r := ev.Eval(expr, nil); r.IsKnown() && !r.IsSensitive() {
					label = "= " + FormatCompact(r.Value, maxLen)
				}
				labels[key] = label
			}
			if label == "" {
				return nil
			}
			hints = append(hints, InlayHint{
				Pos:   exprRng.End,
				Label: label,
				Ref:   TraversalString(expr.Traversal),
			})
			return nil
		})
	}

	for _, block := range body.Blocks {
		switch block.Type {
		case "variable":
			continue
		case "output":
			if isTrue(block.Body.Attributes["sensitive"]) {
				continue
			}
		case "resource", "data":
			if len(block.Labels) == 2 {
				for name, attr := range block.Body.Attributes {
					if ev.isSensitiveArgument(block.Type, block.Labels[0], name) {
						continue
					}
					visit(attr)
				}
				for _, nested := range block.Body.Blocks {
					visit(nested)
				}
				continue
			}
		}
		visit(block)
	}
	if ev.cancelled() {
		return nil
	}
	sort.Slice(hints, func(i, j int) bool {
		return hints[i].Pos.Byte < hints[j].Pos.Byte
	})
	return hints
}

// traversalKey identifies a traversal exactly, index keys included.
func traversalKey(t hcl.Traversal) string {
	var b strings.Builder
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			b.WriteString(s.Name)
		case hcl.TraverseAttr:
			b.WriteString(".")
			b.WriteString(s.Name)
		case hcl.TraverseIndex:
			b.WriteString("[")
			b.WriteString(s.Key.GoString())
			b.WriteString("]")
		case hcl.TraverseSplat:
			b.WriteString("[*]")
		default:
			b.WriteString("?")
		}
	}
	return b.String()
}

// isSensitiveArgument reports whether the provider schema marks an
// argument of a resource or data source type as sensitive.
func (ev *Evaluator) isSensitiveArgument(blockType, typeName, attr string) bool {
	if ev.host == nil || ev.host.Attribute == nil {
		return false
	}
	info, ok := ev.host.Attribute(blockType, typeName, attr)
	return ok && info.Sensitive
}

func isTrue(attr *hclsyntax.Attribute) bool {
	if attr == nil {
		return false
	}
	v, diags := attr.Expr.Value(nil)
	return !diags.HasErrors() && v.IsKnown() && !v.IsNull() && v.Type() == cty.Bool && v.True()
}
