// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package ast

import (
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Run is a run block of a test file and the module it runs.
type Run struct {
	Block *hclsyntax.Block
	Name  string
	// Module is the directory of the module the run runs: the root, where
	// tofu test runs, or the local source of the run's module block. It
	// is "" for a module that is not local.
	Module string
}

// Runs returns the run blocks of a test file for which tofu test runs in
// root. A module block's source is relative to root.
func Runs(body *hclsyntax.Body, root string) []Run {
	runs := make([]Run, 0)
	for _, block := range body.Blocks {
		if block.Type != "run" {
			continue
		}
		run := Run{Block: block, Module: filepath.Clean(root)}
		if len(block.Labels) > 0 {
			run.Name = block.Labels[0]
		}
		for _, nested := range block.Body.Blocks {
			if nested.Type != "module" {
				continue
			}
			run.Module = ""
			src, ok := nested.Body.Attributes["source"]
			if !ok {
				continue
			}
			v, diags := src.Expr.Value(nil)
			if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
				continue
			}
			if s := v.AsString(); strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
				run.Module = filepath.Clean(filepath.Join(root, filepath.FromSlash(s)))
			}
		}
		runs = append(runs, run)
	}
	return runs
}

// ModuleRoot returns the directory where tofu test runs for the test
// files in dir: dir itself when it holds a module, else its parent when
// that holds one (dir is then the tests directory), else dir.
func ModuleRoot(dir string, isModule func(dir string) bool) string {
	dir = filepath.Clean(dir)
	if isModule(dir) {
		return dir
	}
	if parent := filepath.Dir(dir); parent != dir && isModule(parent) {
		return parent
	}
	return dir
}
