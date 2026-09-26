// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package filepaths finds the path arguments of file functions, such as
// templatefile("${path.module}/templates/x.tftpl", ...), and works out the
// files they name when the path is static.
package filepaths

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Kind says what a function's path argument names.
type Kind int

const (
	// File must name an existing file.
	File Kind = iota
	// MaybeFile names a file which may be missing, as in fileexists().
	MaybeFile
	// Directory names a directory, as the first argument of fileset().
	Directory
)

var functions = map[string]Kind{
	"file":             File,
	"filebase64":       File,
	"filebase64sha256": File,
	"filebase64sha512": File,
	"filemd5":          File,
	"filesha1":         File,
	"filesha256":       File,
	"filesha512":       File,
	"templatefile":     File,
	"fileexists":       MaybeFile,
	"fileset":          Directory,
}

// FunctionKind returns what the first argument of the function name is,
// for the functions that read a file or directory by path.
func FunctionKind(name string) (Kind, bool) {
	kind, ok := functions[strings.TrimPrefix(name, "core::")]
	return kind, ok
}

// Call is a call to a function that reads a path.
type Call struct {
	// Function is the function name, without a core:: prefix.
	Function string
	Kind     Kind
	// Path is the path argument.
	Path hclsyntax.Expression
}

// Calls returns the calls to file functions in body, in source order.
func Calls(body hcl.Body) []Call {
	syntaxBody, ok := body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	return calls(syntaxBody)
}

func calls(node hclsyntax.Node) []Call {
	calls := make([]Call, 0)
	hclsyntax.VisitAll(node, func(node hclsyntax.Node) hcl.Diagnostics {
		fc, ok := node.(*hclsyntax.FunctionCallExpr)
		if !ok || len(fc.Args) == 0 {
			return nil
		}
		kind, ok := FunctionKind(fc.Name)
		if !ok {
			return nil
		}
		calls = append(calls, Call{
			Function: strings.TrimPrefix(fc.Name, "core::"),
			Kind:     kind,
			Path:     fc.Args[0],
		})
		return nil
	})
	sort.SliceStable(calls, func(i, j int) bool {
		return calls[i].Path.Range().Start.Byte < calls[j].Path.Range().Start.Byte
	})
	return calls
}

// CallAtPos returns the call whose path argument holds pos. It walks only
// the top-level block or attribute that holds pos.
func CallAtPos(body hcl.Body, pos hcl.Pos) (Call, bool) {
	syntaxBody, ok := body.(*hclsyntax.Body)
	if !ok {
		return Call{}, false
	}
	var item hclsyntax.Node
	for _, b := range syntaxBody.Blocks {
		if b.Range().ContainsPos(pos) {
			item = b
		}
	}
	for _, a := range syntaxBody.Attributes {
		if a.SrcRange.ContainsPos(pos) || a.SrcRange.End.Byte == pos.Byte {
			item = a
		}
	}
	if item == nil {
		return Call{}, false
	}
	for _, c := range calls(item) {
		rng := c.Path.Range()
		if rng.ContainsPos(pos) || rng.End.Byte == pos.Byte {
			return c, true
		}
	}
	return Call{}, false
}

// StaticPath evaluates a path argument that uses nothing but literals,
// path.module and path.root. The path is relative to the module directory
// when anchored, which is when it starts from path.module; otherwise it is
// relative to the root module's directory, where OpenTofu runs, unless it
// is absolute.
func StaticPath(expr hcl.Expression) (path string, anchored bool, ok bool) {
	for _, t := range expr.Variables() {
		if t.RootName() != "path" || len(t) != 2 {
			return "", false, false
		}
		attr, ok := t[1].(hcl.TraverseAttr)
		if !ok {
			return "", false, false
		}
		switch attr.Name {
		case "module":
			anchored = true
		case "root":
		default:
			return "", false, false
		}
	}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"path": cty.ObjectVal(map[string]cty.Value{
				"module": cty.StringVal("."),
				"root":   cty.StringVal("."),
			}),
		},
	}
	v, diags := expr.Value(ctx)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false, false
	}
	path = v.AsString()
	if path == "" {
		return "", false, false
	}
	if filepath.IsAbs(path) {
		anchored = true
	}
	return path, anchored, true
}

// Resolve returns the absolute path of a path argument's value, relative
// to the module directory when anchored and to the root module's directory
// otherwise.
func Resolve(path string, anchored bool, modDir, rootDir string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if anchored || rootDir == "" {
		return filepath.Join(modDir, filepath.FromSlash(path))
	}
	return filepath.Join(rootDir, filepath.FromSlash(path))
}

// TextRange returns the range of a path argument without its quotes, where
// a link fits.
func TextRange(expr hclsyntax.Expression, src []byte) hcl.Range {
	rng := expr.Range()
	switch expr.(type) {
	case *hclsyntax.TemplateExpr, *hclsyntax.TemplateWrapExpr:
	default:
		return rng
	}
	if rng.End.Byte-rng.Start.Byte >= 2 && rng.End.Byte <= len(src) &&
		src[rng.Start.Byte] == '"' && src[rng.End.Byte-1] == '"' {
		rng.Start.Byte++
		rng.Start.Column++
		rng.End.Byte--
		rng.End.Column--
	}
	return rng
}

// LiteralAtPos returns the text of the string literal of a path argument
// which holds pos, from the start of the literal, and whether the text
// follows ${path.module}/ directly. It is false when pos is in an
// interpolation or the argument is not a string template.
func LiteralAtPos(expr hclsyntax.Expression, src []byte, pos hcl.Pos) (typed string, start hcl.Pos, anchored bool, ok bool) {
	if wrap, ok := expr.(*hclsyntax.TemplateWrapExpr); ok {
		// "${path.module}", with the position before the closing quote
		rng := wrap.Range()
		if pos.Byte == rng.End.Byte-1 && src[pos.Byte] == '"' && isPathModule(wrap.Wrapped) {
			return "", pos, true, true
		}
		return "", hcl.Pos{}, false, false
	}
	tpl, isTpl := expr.(*hclsyntax.TemplateExpr)
	if !isTpl {
		return "", hcl.Pos{}, false, false
	}
	rng := TextRange(expr, src)
	if pos.Byte < rng.Start.Byte || pos.Byte > rng.End.Byte {
		return "", hcl.Pos{}, false, false
	}

	var prev hclsyntax.Expression
	for _, part := range tpl.Parts {
		prng := part.Range()
		if pos.Byte >= prng.Start.Byte && pos.Byte <= prng.End.Byte {
			if _, lit := part.(*hclsyntax.LiteralValueExpr); !lit {
				if pos.Byte == prng.Start.Byte && prev == nil {
					// right after the opening quote, before an interpolation
					return "", pos, false, true
				}
				return "", hcl.Pos{}, false, false
			}
			return string(src[prng.Start.Byte:pos.Byte]), prng.Start, isPathModule(prev), true
		}
		prev = part
	}
	if len(tpl.Parts) == 0 || (pos.Byte == rng.Start.Byte) {
		// an empty string
		return "", pos, false, true
	}
	if last := tpl.Parts[len(tpl.Parts)-1]; pos.Byte >= last.Range().End.Byte {
		// after an interpolation, before the closing quote
		return "", pos, isPathModule(last), true
	}
	return "", hcl.Pos{}, false, false
}

func isPathModule(expr hclsyntax.Expression) bool {
	if expr == nil {
		return false
	}
	if w, ok := expr.(*hclsyntax.TemplateWrapExpr); ok {
		expr = w.Wrapped
	}
	t, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(t.Traversal) != 2 || t.Traversal.RootName() != "path" {
		return false
	}
	attr, ok := t.Traversal[1].(hcl.TraverseAttr)
	return ok && attr.Name == "module"
}
