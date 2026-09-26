// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// TestRecord contains what we know about the test files and mock data
// files of a directory.
type TestRecord struct {
	path string

	// RefOrigins are the reference origins of the files as the schema
	// finds them, all local: the path context resolves them against the
	// module each run runs.
	RefOrigins reference.Origins
	// RefTargets are the targets which the files declare themselves,
	// such as provider aliases.
	RefTargets      reference.Targets
	RefErr          error
	RefOriginsState op.OpState

	ParsedFiles     ast.Files
	FilesParsingErr error

	Diagnostics      ast.SourceDiags
	DiagnosticsState globalAst.DiagnosticSourceState
}

func (r *TestRecord) Copy() *TestRecord {
	if r == nil {
		return nil
	}
	newRecord := &TestRecord{
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

func (r *TestRecord) Path() string {
	return r.path
}

// HasTestFiles reports whether the directory has test files, as opposed
// to mock data files only.
func (r *TestRecord) HasTestFiles() bool {
	for name := range r.ParsedFiles {
		if !name.IsMock() {
			return true
		}
	}
	return false
}

// HasMockFiles reports whether the directory has mock data files.
func (r *TestRecord) HasMockFiles() bool {
	for name := range r.ParsedFiles {
		if name.IsMock() {
			return true
		}
	}
	return false
}

func newTestRecord(path string) *TestRecord {
	return &TestRecord{
		path: path,
		DiagnosticsState: globalAst.DiagnosticSourceState{
			globalAst.HCLParsingSource:       op.OpStateUnknown,
			globalAst.SchemaValidationSource: op.OpStateUnknown,
		},
	}
}

// NewTestRecordTest is a test helper to create a new TestRecord
func NewTestRecordTest(path string) *TestRecord {
	return &TestRecord{
		path: path,
	}
}
