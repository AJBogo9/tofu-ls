// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// A type constraint in a hover is shortened when it is longer than
// maxTypeLines or nests objects deeper than maxTypeDepth; big object
// variables otherwise fill a hover with their type alone. Go to Definition
// still shows the whole declaration.
const (
	maxTypeLines = 12
	maxTypeDepth = 2
)

// shortType returns the type constraint src (as written, dedented) or,
// when it is long or deep, a summary of it: objects nested deeper than
// maxTypeDepth become `object({ … N attributes })`, and the attributes of
// the outermost object that do not fit in maxTypeLines lines are counted
// in a last comment line. Comments and the author's layout are kept only
// when nothing is shortened.
func shortType(src string) string {
	lines := strings.Count(src, "\n") + 1
	expr, diags := hclsyntax.ParseExpression([]byte(src), "type.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return src
	}
	// a type a line or two longer than the budget is kept whole: cutting
	// it would save little and hide attributes
	if lines <= maxTypeLines+2 && objectDepth(expr) <= maxTypeDepth {
		return src
	}
	r := &typeRenderer{src: []byte(src)}
	return r.render(expr, "", 0)
}

// objectDepth is how deeply object type constraints nest in expr.
func objectDepth(expr hclsyntax.Expression) int {
	call, ok := expr.(*hclsyntax.FunctionCallExpr)
	if !ok {
		return 0
	}
	deepest := 0
	for _, arg := range call.Args {
		if obj, ok := arg.(*hclsyntax.ObjectConsExpr); ok && call.Name == "object" {
			for _, item := range obj.Items {
				if d := objectDepth(item.ValueExpr); d > deepest {
					deepest = d
				}
			}
			continue
		}
		if tup, ok := arg.(*hclsyntax.TupleConsExpr); ok {
			for _, e := range tup.Exprs {
				if d := objectDepth(e); d > deepest {
					deepest = d
				}
			}
			continue
		}
		if d := objectDepth(arg); d > deepest {
			deepest = d
		}
	}
	if call.Name == "object" {
		return deepest + 1
	}
	return deepest
}

type typeRenderer struct {
	src []byte
	// capped is set once the outermost object has been rendered, which is
	// the one whose attributes the line budget applies to.
	capped bool
}

// render writes expr as a type constraint starting at the current column,
// with nested lines indented by indent. depth counts the objects around it.
func (r *typeRenderer) render(expr hclsyntax.Expression, indent string, depth int) string {
	call, ok := expr.(*hclsyntax.FunctionCallExpr)
	if !ok {
		return r.oneLine(expr)
	}
	switch {
	case call.Name == "object" && len(call.Args) == 1:
		obj, ok := call.Args[0].(*hclsyntax.ObjectConsExpr)
		if !ok {
			return r.oneLine(expr)
		}
		return r.object(obj, indent, depth)
	case call.Name == "tuple" && len(call.Args) == 1:
		tup, ok := call.Args[0].(*hclsyntax.TupleConsExpr)
		if !ok {
			return r.oneLine(expr)
		}
		parts := make([]string, 0, len(tup.Exprs))
		for _, e := range tup.Exprs {
			parts = append(parts, r.render(e, indent, depth))
		}
		return "tuple([" + strings.Join(parts, ", ") + "])"
	case len(call.Args) >= 1:
		// list, set, map and optional, with optional's default as written
		s := call.Name + "(" + r.render(call.Args[0], indent, depth)
		for _, arg := range call.Args[1:] {
			s += ", " + r.oneLine(arg)
		}
		return s + ")"
	}
	return r.oneLine(expr)
}

func (r *typeRenderer) object(obj *hclsyntax.ObjectConsExpr, indent string, depth int) string {
	if len(obj.Items) == 0 {
		return "object({})"
	}
	if depth >= maxTypeDepth {
		return fmt.Sprintf("object({ … %d %s })", len(obj.Items), plural(len(obj.Items), "attribute", "attributes"))
	}
	outermost := !r.capped
	r.capped = true

	inner := indent + "  "
	type item struct {
		key, value string
	}
	items := make([]item, 0, len(obj.Items))
	for _, it := range obj.Items {
		items = append(items, item{key: r.oneLine(it.KeyExpr), value: r.render(it.ValueExpr, inner, depth+1)})
	}

	// the first line, the closing line and a line saying what is left out
	budget := maxTypeLines - 3
	kept := len(items)
	if outermost {
		used := 0
		for i, it := range items {
			n := strings.Count(it.value, "\n") + 1
			if used+n > budget {
				kept = i
				break
			}
			used += n
		}
	}

	var b strings.Builder
	b.WriteString("object({\n")
	// align the = of consecutive one-line attributes, as tofu fmt does
	for i := 0; i < kept; {
		j := i
		width := 0
		for j < kept && !strings.Contains(items[j].value, "\n") {
			if len(items[j].key) > width {
				width = len(items[j].key)
			}
			j++
		}
		if j == i {
			fmt.Fprintf(&b, "%s%s = %s\n", inner, items[i].key, items[i].value)
			i++
			continue
		}
		for ; i < j; i++ {
			fmt.Fprintf(&b, "%s%-*s = %s\n", inner, width, items[i].key, items[i].value)
		}
	}
	if kept < len(items) {
		n := len(items) - kept
		fmt.Fprintf(&b, "%s# … %d more %s (Go to Definition shows the full type)\n", inner, n, plural(n, "attribute", "attributes"))
	}
	b.WriteString(indent + "})")
	return b.String()
}

// oneLine is the source of expr with its whitespace collapsed.
func (r *typeRenderer) oneLine(expr hclsyntax.Expression) string {
	rng := expr.Range()
	if rng.End.Byte > len(r.src) || rng.Start.Byte > rng.End.Byte {
		return ""
	}
	return strings.Join(strings.Fields(string(rng.SliceBytes(r.src))), " ")
}

// maxDescriptionLines caps a variable's description in a hover. Some
// modules document every attribute of an object variable there.
const maxDescriptionLines = 16

// shortDescription keeps the first lines of a long description, ending
// at a paragraph break when there is one nearby, and says how many lines
// are left out. A code fence left open is closed.
func shortDescription(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= maxDescriptionLines {
		return s
	}
	keep := maxDescriptionLines
	for i := keep; i > keep/2; i-- {
		if strings.TrimSpace(lines[i-1]) == "" {
			keep = i - 1
			break
		}
	}
	kept := lines[:keep]
	fences := 0
	for _, l := range kept {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fences++
		}
	}
	out := strings.Join(kept, "\n")
	if fences%2 == 1 {
		out += "\n```"
	}
	n := len(lines) - keep
	return out + fmt.Sprintf("\n\n_… %d more %s of description (Go to Definition shows them)_", n, plural(n, "line", "lines"))
}
