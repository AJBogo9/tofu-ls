// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// TerragruntRecord contains what we know about the Terragrunt files of a
// directory.
type TerragruntRecord struct {
	path string

	// RefOrigins are the reference origins of the files.
	RefOrigins reference.Origins
	// RefTargets are the targets the files declare themselves: local
	// values, dependencies, feature flags, units and stacks. Targets in
	// included files are added by the path context.
	RefTargets      reference.Targets
	RefErr          error
	RefOriginsState op.OpState

	ParsedFiles     ast.Files
	Languages       ast.Languages
	FilesParsingErr error

	Diagnostics      ast.SourceDiags
	DiagnosticsState globalAst.DiagnosticSourceState
}

func (r *TerragruntRecord) Copy() *TerragruntRecord {
	if r == nil {
		return nil
	}
	newRecord := &TerragruntRecord{
		path: r.path,

		RefOrigins:      r.RefOrigins.Copy(),
		RefTargets:      r.RefTargets.Copy(),
		RefErr:          r.RefErr,
		RefOriginsState: r.RefOriginsState,

		FilesParsingErr: r.FilesParsingErr,

		DiagnosticsState: r.DiagnosticsState.Copy(),
	}

	if r.ParsedFiles != nil {
		// hcl.File is practically immutable once it comes out of parser
		newRecord.ParsedFiles = r.ParsedFiles.Copy()
	}
	if r.Languages != nil {
		newRecord.Languages = r.Languages.Copy()
	}

	if r.Diagnostics != nil {
		newRecord.Diagnostics = make(ast.SourceDiags, len(r.Diagnostics))
		for source, diags := range r.Diagnostics {
			newRecord.Diagnostics[source] = make(ast.Diags, len(diags))
			for name, d := range diags {
				newRecord.Diagnostics[source][name] = make(hcl.Diagnostics, len(d))
				copy(newRecord.Diagnostics[source][name], d)
			}
		}
	}

	return newRecord
}

func (r *TerragruntRecord) Path() string {
	return r.path
}

// FilesOf returns the parsed files of one language (terragrunt or
// terragrunt-stack).
func (r *TerragruntRecord) FilesOf(language string) map[string]*hcl.File {
	files := make(map[string]*hcl.File)
	for name, f := range r.ParsedFiles {
		if r.Languages[name] == language {
			files[name.String()] = f
		}
	}
	return files
}

// HasLanguage reports whether the directory has files of the language.
func (r *TerragruntRecord) HasLanguage(language string) bool {
	for name := range r.ParsedFiles {
		if r.Languages[name] == language {
			return true
		}
	}
	return false
}

func newTerragruntRecord(path string) *TerragruntRecord {
	return &TerragruntRecord{
		path: path,
		DiagnosticsState: globalAst.DiagnosticSourceState{
			globalAst.HCLParsingSource:       op.OpStateUnknown,
			globalAst.SchemaValidationSource: op.OpStateUnknown,
		},
	}
}

// NewTerragruntRecordTest is a test helper to create a new TerragruntRecord
func NewTerragruntRecordTest(path string) *TerragruntRecord {
	return &TerragruntRecord{
		path: path,
	}
}
