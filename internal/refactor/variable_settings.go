// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Setting is a place outside a module which sets one of its variables.
type Setting struct {
	// File is the absolute path of the file.
	File string
	// Range is what removing the setting deletes: the attribute, or a
	// test file's variables block when the attribute is all it holds.
	Range hcl.Range
}

// VariableSettings returns the places outside the module at path which
// set its variable name, found as rename finds them:
//   - keys of the var files next to the module and in its subdirectories
//     (JSON var files are left out: an undeclared variable there is only
//     a warning);
//   - keys of the variables blocks of test files which apply to runs of
//     the module, unless a run of another module reads the same key;
//   - arguments of the module calls which call the module through a
//     local source.
//
// It fails when one of those files has syntax errors, since a setting in
// it could be missed.
func VariableSettings(ctx context.Context, env Env, path lang.Path, name string) ([]Setting, error) {
	modPath := filepath.Clean(path.Path)
	fc := newFileCache(env)
	fc.base = modPath
	settings := make([]Setting, 0)

	parsed := func(file string) (*hclsyntax.Body, error) {
		src, err := fc.source(file)
		if err != nil {
			return nil, err
		}
		f, diags := hclsyntax.ParseConfig(src, filepath.Base(file), hcl.InitialPos)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s has syntax errors", fc.displayPath(file))
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			return nil, fmt.Errorf("unexpected body in %s", filepath.Base(file))
		}
		return body, nil
	}

	// var files
	varFiles := make([]string, 0)
	if env.ReadDir != nil {
		entries, err := env.ReadDir(modPath)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".tfvars") {
					varFiles = append(varFiles, filepath.Join(modPath, e.Name()))
				}
			}
		}
	}
	for _, file := range subdirVarFiles(env, modPath) {
		if !strings.HasSuffix(file, ".json") {
			varFiles = append(varFiles, file)
		}
	}
	sort.Strings(varFiles)
	for _, file := range varFiles {
		body, err := parsed(file)
		if err != nil {
			return nil, err
		}
		if attr, ok := body.Attributes[name]; ok {
			settings = append(settings, Setting{File: file, Range: attr.SrcRange})
		}
	}

	// test files
	roots := []string{modPath}
	for _, p := range env.PathReader.Paths(ctx) {
		if dir := filepath.Clean(p.Path); p.LanguageID == path.LanguageID && dir != modPath && !isInstalledModule(dir) {
			roots = append(roots, dir)
		}
	}
	sort.Strings(roots[1:])
	seen := map[string]bool{}
	for _, root := range roots {
		for _, file := range moduleTestFiles(env, root) {
			if seen[file] {
				continue
			}
			seen[file] = true
			body, err := parsed(file)
			if err != nil {
				return nil, err
			}
			runs := testRuns(body, root)
			ownRuns := 0
			for _, run := range runs {
				if run.module == modPath {
					ownRuns++
				}
			}
			if root != modPath && ownRuns == 0 {
				continue
			}

			// the file's variables are given to every run
			if ownRuns > 0 || len(runs) == 0 {
				if _, other := runReadingFileVariable(env, runs, modPath, name, path.LanguageID); !other {
					settings = append(settings, variablesSettings(file, body, name)...)
				}
			}
			for _, run := range runs {
				if run.module == modPath {
					settings = append(settings, variablesSettings(file, run.block.Body, name)...)
				}
			}
		}
	}

	// module calls
	for _, p := range env.PathReader.Paths(ctx) {
		dir := filepath.Clean(p.Path)
		if p.LanguageID != path.LanguageID || dir == modPath || isInstalledModule(dir) {
			continue
		}
		calls := callsOfModule(env, dir, modPath)
		if len(calls) == 0 {
			continue
		}
		declared, err := env.ModuleCalls(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(calls))
		for call := range calls {
			names = append(names, call)
		}
		sort.Strings(names)
		for _, call := range names {
			decl, ok := declared[call]
			if !ok || decl.RangePtr == nil {
				continue
			}
			file := decl.RangePtr.Filename
			if !filepath.IsAbs(file) {
				file = filepath.Join(dir, file)
			}
			if strings.HasSuffix(file, ".json") {
				return nil, fmt.Errorf("module.%s is called from JSON configuration (%s)", call, fc.displayPath(file))
			}
			body, err := parsed(file)
			if err != nil {
				return nil, err
			}
			for _, block := range body.Blocks {
				if block.Type != "module" || len(block.Labels) != 1 || block.Labels[0] != call {
					continue
				}
				if attr, ok := block.Body.Attributes[name]; ok {
					settings = append(settings, Setting{File: file, Range: attr.SrcRange})
				}
			}
		}
	}
	return settings, nil
}

// variablesSettings returns the keys called name in the variables blocks
// directly in body, or the whole block where the key is all it holds.
func variablesSettings(file string, body *hclsyntax.Body, name string) []Setting {
	var settings []Setting
	for _, block := range body.Blocks {
		if block.Type != "variables" {
			continue
		}
		attr, ok := block.Body.Attributes[name]
		if !ok {
			continue
		}
		rng := attr.SrcRange
		if len(block.Body.Attributes) == 1 && len(block.Body.Blocks) == 0 {
			rng = block.Range()
		}
		settings = append(settings, Setting{File: file, Range: rng})
	}
	return settings
}
