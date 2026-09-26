// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/refactor"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// UnusedSymbols reports variables and locals which nothing in the module
// references, as hints which clients render faded.
//
// A variable set in a .tfvars file but not used in code is still unused,
// and the message says where it is set.
//
// Nothing is reported unless every file of the module parsed cleanly,
// since references in a broken file could be missed.
func UnusedSymbols(ctx context.Context, fs ReadOnlyFS, modStore *state.ModuleStore, modPath string) error {
	mod, err := modStore.ModuleRecordByPath(modPath)
	if err != nil {
		return err
	}

	diags := make(ast.ModDiags, 0)
	for name := range mod.ParsedModuleFiles {
		// an empty entry clears what was reported before
		diags[name] = hcl.Diagnostics{}
	}

	complete := mod.ModuleParsingErr == nil && len(mod.ParsedModuleFiles) > 0
	for _, fileDiags := range mod.ModuleDiagnostics[globalAst.HCLParsingSource] {
		if fileDiags.HasErrors() {
			complete = false
		}
	}

	if complete {
		files := make(map[string]*hcl.File, len(mod.ParsedModuleFiles))
		for name, file := range mod.ParsedModuleFiles {
			if name.IsIgnored() {
				continue
			}
			files[name.String()] = file
		}

		tfvars := tfvarsKeys(fs, modPath)
		for _, sym := range refactor.UnusedSymbols(files) {
			var summary, code string
			switch sym.Kind {
			case refactor.KindVariable:
				summary = fmt.Sprintf("Variable %q is declared but not used in this module", sym.Name)
				if setIn := tfvars[sym.Name]; len(setIn) > 0 {
					summary += fmt.Sprintf(" (only set in %s)", strings.Join(setIn, ", "))
				}
				code = ilsp.CodeUnusedVariable
			case refactor.KindLocal:
				summary = fmt.Sprintf("Local value %q is declared but not used", sym.Name)
				code = ilsp.CodeUnusedLocal
			}
			rng := sym.NameRange
			name := ast.ModFilename(sym.Filename)
			diags[name] = append(diags[name], &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  summary,
				Subject:  &rng,
				Extra: ilsp.CodedDiagnostic{
					Code:        code,
					Data:        map[string]interface{}{"name": sym.Name},
					Unnecessary: true,
				},
			})
		}
	}

	return modStore.UpdateModuleDiagnostics(modPath, globalAst.UnusedSymbolsSource, diags)
}

// tfvarsKeys returns, for each variable set in a .tfvars file of the
// directory, the names of the files which set it.
func tfvarsKeys(fs ReadOnlyFS, modPath string) map[string][]string {
	keys := make(map[string][]string, 0)

	entries, err := fs.ReadDir(modPath)
	if err != nil {
		return keys
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".tfvars") {
			continue
		}
		src, err := fs.ReadFile(filepath.Join(modPath, name))
		if err != nil {
			continue
		}
		f, _ := hclsyntax.ParseConfig(src, name, hcl.InitialPos)
		if f == nil {
			continue
		}
		attrs, _ := f.Body.JustAttributes()
		for key := range attrs {
			keys[key] = append(keys[key], name)
		}
	}
	for key := range keys {
		sort.Strings(keys[key])
	}

	return keys
}
