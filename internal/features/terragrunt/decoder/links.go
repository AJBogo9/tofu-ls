// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/paths"
)

// Link is a path in a Terragrunt file which names an existing file.
type Link struct {
	// Range is the path expression in the Terragrunt file.
	Range hcl.Range
	// Target is the absolute path of the file to open: the included
	// file, the configuration of a dependency, or a file of a local
	// module source.
	Target  string
	Tooltip string
}

// FileLinks returns the links of a Terragrunt file in dir: include paths,
// dependency config_path and dependencies.paths (to the unit's
// terragrunt.hcl or the stack's terragrunt.stack.hcl), local sources of
// the terraform block and of stack units (to main.tf, or the unit's
// configuration), and the files of find_in_parent_folders and
// read_terragrunt_config. Only static paths that
// name existing files are links; relative paths only in entry files.
func FileLinks(fsys paths.FS, dir, filename string, file *hcl.File) []Link {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	r := paths.NewResolver(fsys, dir, file, ast.IsEntryFilename(filename))
	links := make([]Link, 0)
	seen := make(map[hcl.Range]bool)
	add := func(expr hclsyntax.Expression, target string, ok bool) {
		if !ok {
			return
		}
		rng := paths.TextRange(expr, file.Bytes)
		if seen[rng] {
			return
		}
		seen[rng] = true
		rel, err := filepath.Rel(dir, target)
		if err != nil {
			rel = target
		}
		links = append(links, Link{
			Range:   rng,
			Target:  target,
			Tooltip: fmt.Sprintf("Open %s", filepath.ToSlash(rel)),
		})
	}
	file_ := func(expr hclsyntax.Expression) (string, bool) {
		path, ok := r.Path(expr)
		if !ok {
			return "", false
		}
		return r.File(path)
	}
	config := func(expr hclsyntax.Expression) (string, bool) {
		path, ok := r.Path(expr)
		if !ok {
			return "", false
		}
		return r.ConfigIn(path)
	}

	for _, block := range body.Blocks {
		attrs := block.Body.Attributes
		switch block.Type {
		case "include":
			if attr, ok := attrs["path"]; ok {
				target, ok := file_(attr.Expr)
				add(attr.Expr, target, ok)
			}
		case "dependency":
			if attr, ok := attrs["config_path"]; ok {
				target, ok := config(attr.Expr)
				add(attr.Expr, target, ok)
			}
		case "dependencies":
			if attr, ok := attrs["paths"]; ok {
				if tuple, ok := attr.Expr.(*hclsyntax.TupleConsExpr); ok {
					for _, elem := range tuple.Exprs {
						target, ok := config(elem)
						add(elem, target, ok)
					}
				}
			}
		case "terraform":
			if attr, ok := attrs["source"]; ok {
				if dir, ok := r.LocalSource(attr.Expr); ok {
					target, ok := r.ModuleFileIn(dir)
					add(attr.Expr, target, ok)
				}
			}
		case "unit", "stack":
			if attr, ok := attrs["source"]; ok {
				if dir, ok := r.LocalSource(attr.Expr); ok {
					target, ok := r.ConfigIn(dir)
					add(attr.Expr, target, ok)
				}
			}
		}
	}

	// files that functions read, anywhere in the file
	_ = hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		call, ok := node.(*hclsyntax.FunctionCallExpr)
		if !ok {
			return nil
		}
		switch {
		case call.Name == "find_in_parent_folders":
			target, ok := file_(call)
			add(call, target, ok)
		case call.Name == "read_terragrunt_config" && len(call.Args) > 0:
			// relative to this file, and a directory stands for its
			// configuration (the other read functions read relative to
			// the working directory, which may be a download)
			target, ok := config(call.Args[0])
			add(call.Args[0], target, ok)
		}
		return nil
	})

	// a path inside another one, such as the find_in_parent_folders()
	// in "${dirname(find_in_parent_folders("root.hcl"))}/common.hcl",
	// is left out: the whole path is what the attribute names
	outer := make([]Link, 0, len(links))
	for i, link := range links {
		inside := false
		for j, other := range links {
			if i != j && other.Range.Start.Byte <= link.Range.Start.Byte && link.Range.End.Byte <= other.Range.End.Byte &&
				(other.Range.Start.Byte != link.Range.Start.Byte || other.Range.End.Byte != link.Range.End.Byte) {
				inside = true
				break
			}
		}
		if !inside {
			outer = append(outer, link)
		}
	}
	sort.SliceStable(outer, func(i, j int) bool { return outer[i].Range.Start.Byte < outer[j].Range.Start.Byte })
	return outer
}

// LinkAt returns the link at pos.
func LinkAt(links []Link, pos hcl.Pos) (Link, bool) {
	for _, link := range links {
		if link.Range.ContainsPos(pos) || link.Range.End == pos {
			return link, true
		}
	}
	return Link{}, false
}
