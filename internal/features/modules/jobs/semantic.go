// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfmod "github.com/opentofu/opentofu-schema/module"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/opentofu/tofu-ls/internal/checks"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/modules/decoder"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/settings"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// SemanticValidation runs the checks which need the whole module (see
// package checks): duplicate declarations, unresolved references,
// unknown resource types, variable types, tfvars values, static-only
// contexts, operand types, installation, unused data sources and
// interpolation-only templates. The families switched off in opts are
// skipped.
//
// It relies on the parsed files and metadata of the module and of the
// modules it calls ([LoadModuleMetadata]) and on the provider schemas
// loaded so far. Its diagnostics include those of the directory's
// .tfvars files, which it reads from fs, so that they always match the
// module's current variables.
func SemanticValidation(ctx context.Context, fs ReadOnlyFS, modStore *state.ModuleStore, rootFeature fdecoder.RootReader,
	schemaStore *globalState.ProviderSchemaStore, modPath string, opts settings.ValidationOptions) error {
	mod, err := modStore.ModuleRecordByPath(modPath)
	if err != nil {
		return err
	}

	// Avoid validation if it is already in progress or already finished
	if mod.ModuleDiagnosticsState[globalAst.SemanticValidationSource] != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(modPath)}
	}

	err = modStore.SetModuleDiagnosticsState(modPath, globalAst.SemanticValidationSource, op.OpStateLoading)
	if err != nil {
		return err
	}

	cmod := &checks.Module{
		Path:       modPath,
		Files:      make(map[string]*hcl.File, len(mod.ParsedModuleFiles)),
		Broken:     make(map[string]bool),
		VarsFiles:  make(map[string]*hcl.File),
		BrokenVars: make(map[string]bool),
		Meta: &tfmod.Meta{
			Path:                 modPath,
			ProviderReferences:   mod.Meta.ProviderReferences,
			ProviderRequirements: mod.Meta.ProviderRequirements,
			Variables:            mod.Meta.Variables,
			Outputs:              mod.Meta.Outputs,
			ModuleCalls:          mod.Meta.ModuleCalls,
		},
		ChildOutputs: childOutputs(modStore, rootFeature, modPath, mod.Meta.ModuleCalls),
		Schema:       lockedSchemaReader(schemaStore, modPath),
		FS:           fs,
	}
	for name, f := range mod.ParsedModuleFiles {
		if name.IsIgnored() {
			continue
		}
		cmod.Files[name.String()] = f
	}
	if mod.ModuleParsingErr != nil {
		for name := range cmod.Files {
			cmod.Broken[name] = true
		}
	}
	for name, fileDiags := range mod.ModuleDiagnostics[globalAst.HCLParsingSource] {
		if fileDiags.HasErrors() {
			cmod.Broken[name.String()] = true
		}
	}
	readVarsFiles(fs, modPath, cmod)

	diags := checks.Run(cmod, opts)

	modDiags := make(ast.ModDiags, len(cmod.Files)+len(cmod.VarsFiles))
	for name := range cmod.Files {
		// an empty entry clears what was reported before
		modDiags[ast.ModFilename(name)] = hcl.Diagnostics{}
	}
	for name := range cmod.VarsFiles {
		modDiags[ast.ModFilename(name)] = hcl.Diagnostics{}
	}
	for name, fileDiags := range diags {
		modDiags[ast.ModFilename(name)] = fileDiags
	}

	return modStore.UpdateModuleDiagnostics(modPath, globalAst.SemanticValidationSource, modDiags)
}

// readVarsFiles parses the .tfvars files of the module's directory.
func readVarsFiles(fs ReadOnlyFS, modPath string, cmod *checks.Module) {
	entries, err := fs.ReadDir(modPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".tfvars") || globalAst.IsIgnoredFile(name) {
			continue
		}
		src, err := fs.ReadFile(filepath.Join(modPath, name))
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(src, name, hcl.InitialPos)
		if f == nil {
			continue
		}
		cmod.VarsFiles[name] = f
		if diags.HasErrors() {
			cmod.BrokenVars[name] = true
		}
	}
}

// childOutputs returns the outputs of the modules which the module
// calls, for each call whose module is indexed, has its metadata loaded
// and parsed cleanly.
func childOutputs(modStore *state.ModuleStore, rootFeature fdecoder.RootReader, modPath string, calls map[string]tfmod.DeclaredModuleCall) map[string]map[string]bool {
	outputs := make(map[string]map[string]bool)
	for name, mc := range calls {
		var childPath string
		switch source := mc.SourceAddr.(type) {
		case tfmod.LocalSourceAddr:
			childPath = filepath.Join(modPath, filepath.FromSlash(source.String()))
		case tfaddr.Module, tfmod.RemoteSourceAddr:
			installedDir, ok := rootFeature.InstalledModulePath(modPath, source.String())
			if !ok {
				continue
			}
			childPath = filepath.Join(modPath, filepath.FromSlash(installedDir))
		default:
			continue
		}
		child, err := modStore.ModuleRecordByPath(childPath)
		if err != nil || child.MetaState != op.OpStateLoaded || child.ModuleParsingErr != nil || len(child.ParsedModuleFiles) == 0 {
			continue
		}
		broken := false
		for _, fileDiags := range child.ModuleDiagnostics[globalAst.HCLParsingSource] {
			if fileDiags.HasErrors() {
				broken = true
			}
		}
		if broken {
			continue
		}
		names := make(map[string]bool, len(child.Meta.Outputs))
		for out := range child.Meta.Outputs {
			names[out] = true
		}
		outputs[name] = names
	}
	return outputs
}

// lockedSchemaReader returns provider schemas which match a locked
// version exactly: the one tofu init installed for this module (from
// tofu providers schema), else a bundled schema of the same version.
func lockedSchemaReader(schemaStore *globalState.ProviderSchemaStore, modPath string) checks.SchemaReader {
	return func(addr tfaddr.Provider, v *version.Version) *tfschema.ProviderSchema {
		if schemaStore == nil || v == nil {
			return nil
		}
		it, err := schemaStore.ListSchemas()
		if err != nil {
			return nil
		}
		var bundled *tfschema.ProviderSchema
		for ps := it.Next(); ps != nil; ps = it.Next() {
			if ps.Schema == nil || !ps.Address.Equals(addr) {
				continue
			}
			switch src := ps.Source.(type) {
			case globalState.LocalSchemaSource:
				if src.ModulePath == modPath {
					return ps.Schema
				}
			case globalState.PreloadedSchemaSource:
				if ps.Version != nil && ps.Version.Equal(v) {
					bundled = ps.Schema
				}
			}
		}
		return bundled
	}
}
