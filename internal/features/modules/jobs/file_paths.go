// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/filepaths"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// MissingPathFiles warns about file function paths, as in templatefile()
// or file(), that are static and name no file, and fileset() directories
// that do not exist. fileexists() is never reported.
//
// Only paths whose file is certain are checked: those built from literals
// and path.module, and absolute ones. A path relative to the root module
// is checked only in a module that no indexed module calls and that is not
// installed below .terraform, since a caller's directory is where
// OpenTofu resolves it. Paths built from variables or locals are not
// checked, since a variable can be set differently when OpenTofu runs.
func MissingPathFiles(ctx context.Context, fs ReadOnlyFS, modStore *state.ModuleStore, modPath string, isCalled func() bool) error {
	mod, err := modStore.ModuleRecordByPath(modPath)
	if err != nil {
		return err
	}

	diags := make(ast.ModDiags, 0)
	found := 0
	calledKnown, called := false, false
	for name, file := range mod.ParsedModuleFiles {
		if name.IsIgnored() || name.IsJSON() {
			continue
		}
		// an empty entry clears what was reported before
		diags[name] = hcl.Diagnostics{}
		for _, call := range filepaths.Calls(file.Body) {
			if call.Kind == filepaths.MaybeFile {
				continue
			}
			path, anchored, ok := filepaths.StaticPath(call.Path)
			if !ok {
				continue
			}
			if !anchored {
				if !calledKnown {
					calledKnown = true
					called = isInstalledModule(modPath) || (isCalled != nil && isCalled())
				}
				if called {
					continue
				}
			}
			resolved := filepaths.Resolve(path, anchored, modPath, "")
			fi, err := fs.Stat(resolved)
			wantDir := call.Kind == filepaths.Directory
			if err == nil && fi.IsDir() == wantDir {
				continue
			}

			shown := resolved
			if rel, err := filepath.Rel(modPath, resolved); err == nil && !strings.HasPrefix(rel, "..") {
				shown = filepath.ToSlash(rel)
			}
			var summary, detail string
			switch {
			case err == nil && wantDir:
				summary = fmt.Sprintf("%q is a file, not a directory", shown)
				detail = fmt.Sprintf("%s() reads a directory.", call.Function)
			case err == nil:
				summary = fmt.Sprintf("%q is a directory, not a file", shown)
				detail = fmt.Sprintf("%s() reads a file.", call.Function)
			case wantDir:
				summary = fmt.Sprintf("Directory %q not found", shown)
			default:
				summary = fmt.Sprintf("File %q not found", shown)
			}
			rng := call.Path.Range()
			diags[name] = append(diags[name], &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  summary,
				Detail:   detail,
				Subject:  &rng,
				Extra: ilsp.CodedDiagnostic{
					Code: ilsp.CodeTemplateFileMissing,
					Data: map[string]interface{}{
						"path":     resolved,
						"function": call.Function,
					},
				},
			})
			found++
		}
	}

	reported := 0
	for _, fileDiags := range mod.ModuleDiagnostics[globalAst.FilePathsSource] {
		reported += len(fileDiags)
	}
	if found == 0 && reported == 0 {
		// nothing reported before or now: spare the diagnostics refresh
		return nil
	}
	return modStore.UpdateModuleDiagnostics(modPath, globalAst.FilePathsSource, diags)
}

func isInstalledModule(modPath string) bool {
	return strings.Contains(filepath.ToSlash(modPath), "/.terraform/modules/")
}
