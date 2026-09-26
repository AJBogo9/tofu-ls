// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver/cmd"
	"github.com/opentofu/tofu-ls/internal/langserver/diagnostics"
	"github.com/opentofu/tofu-ls/internal/settings"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/tofu/ast"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// staticFS is the filesystem static values are loaded from: every module
// gets the -var-file files and TF_VAR_ variables chosen for it.
func (svc *service) staticFS() staticval.FS {
	if svc.valueInputs == nil {
		return svc.fs
	}
	return staticval.WithInputs(svc.fs, svc.valueInputs)
}

// setValueInputs replaces the chosen inputs. It returns the module
// directories whose -var-file choice changed, and whether the TF_VAR_
// variables changed.
func (svc *service) setValueInputs(opts settings.Values) ([]string, bool) {
	if svc.valueInputs == nil {
		svc.valueInputs = staticval.NewInputsStore()
	}
	varFiles := make(map[string][]string, len(opts.VarFiles))
	for dir, files := range opts.VarFiles {
		path := dir
		if strings.HasPrefix(dir, "file:") {
			p, err := uri.PathFromURI(dir)
			if err != nil {
				continue
			}
			path = p
		}
		if !filepath.IsAbs(path) {
			continue
		}
		varFiles[path] = files
	}
	var envVars map[string]string
	if opts.ReadEnvironment {
		envVars = staticval.EnvVarsFrom(os.Environ())
	}
	return svc.valueInputs.Set(varFiles, envVars)
}

// valuesInputsHandler answers tofu-ls.values.inputs, with which the
// client chooses the inputs of static values as a run of tofu gets them:
//
//   - varFiles: a JSON object from module directory (URI or path) to its
//     -var-file files, relative to the module, in order;
//   - readEnvironment: whether the TF_VAR_ variables of the language
//     server's environment are read.
//
// The choice replaces the previous one. The modules whose inputs changed,
// and the local modules they call, are validated again, and the client
// is asked for fresh inlay hints.
func (svc *service) valuesInputsHandler(ctx context.Context, args cmd.CommandArgs) (interface{}, error) {
	var opts settings.Values
	if raw, ok := args.GetString("varfiles"); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts.VarFiles); err != nil {
			return nil, fmt.Errorf("%w: varFiles is not a JSON object of file lists: %s", jrpc2.InvalidParams.Err(), err)
		}
	}
	opts.ReadEnvironment, _ = args.GetBool("readenvironment")

	changed, envChanged := svc.setValueInputs(opts)
	if svc.features == nil || svc.features.Modules == nil {
		return nil, nil
	}
	dirs := make(map[string]bool)
	for _, dir := range changed {
		dirs[dir] = true
	}
	if envChanged {
		// TF_VAR_ variables concern every root module: validate the
		// modules being edited again
		records, err := svc.features.Modules.Store.List()
		if err == nil {
			for _, rec := range records {
				if open, err := svc.stateStore.DocumentStore.HasOpenDocuments(document.DirHandleFromPath(rec.Path())); err == nil && open {
					dirs[rec.Path()] = true
				}
			}
		}
	}
	// a child module's values come from its callers
	for dir := range dirs {
		calls, err := svc.features.Modules.Store.DeclaredModuleCalls(dir)
		if err != nil {
			continue
		}
		for _, mc := range calls {
			src := mc.RawSourceAddr
			if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
				dirs[filepath.Clean(filepath.Join(dir, src))] = true
			}
		}
	}
	sorted := make([]string, 0, len(dirs))
	for dir := range dirs {
		sorted = append(sorted, dir)
	}
	sort.Strings(sorted)
	for _, dir := range sorted {
		if _, err := svc.features.Modules.Revalidate(ctx, dir); err != nil {
			svc.logger.Printf("validating %q with its new inputs: %s", dir, err)
		}
	}
	svc.scheduleInlayHintRefresh()
	return nil, nil
}

// extendVarFileDiags completes the diagnostics published for path with
// those of -var-file files that live in another directory than their
// module, such as envs/prod.tfvars. Such a file's diagnostics come from
// two records: the module's checks and its own directory's (syntax
// errors). A publish replaces every diagnostic of a file, so each publish
// carries both.
func extendVarFileDiags(features *Features, inputs *staticval.InputsStore, path string, diags diagnostics.Diagnostics) {
	if inputs == nil || features == nil || features.Modules == nil || features.Variables == nil {
		return
	}
	// the directory of the file: add what its modules report on it
	for _, modDir := range inputs.Selecting(path) {
		if modDir == path {
			continue
		}
		for name, bySource := range features.Modules.Diagnostics(modDir) {
			if name == "" || filepath.Dir(filepath.Join(modDir, filepath.FromSlash(name))) != filepath.Clean(path) {
				continue
			}
			base := filepath.Base(filepath.FromSlash(name))
			extendFile(diags, base, bySource)
		}
	}
	// the module: add what the file's directory reports on it, for every
	// file of a subdirectory reported now (chosen, or being cleared)
	for name := range diags {
		if !strings.Contains(name, "/") {
			continue
		}
		file := filepath.Join(path, filepath.FromSlash(name))
		fileDiags, ok := features.Variables.Diagnostics(filepath.Dir(file))[filepath.Base(file)]
		if ok {
			extendFile(diags, name, fileDiags)
		}
	}
}

func extendFile(diags diagnostics.Diagnostics, name string, bySource map[ast.DiagnosticSource]hcl.Diagnostics) {
	if _, ok := diags[name]; !ok {
		diags[name] = make(map[ast.DiagnosticSource]hcl.Diagnostics)
	}
	for src, d := range bySource {
		diags[name][src] = append(diags[name][src], d...)
	}
}
