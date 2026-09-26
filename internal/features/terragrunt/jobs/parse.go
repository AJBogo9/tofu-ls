// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"io/fs"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/parser"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/job"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

type ReadOnlyFS interface {
	fs.FS
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
}

// ParseTerragruntFiles parses the Terragrunt files of a directory. The
// files are few and small, so every file is parsed again on any change.
// openLanguages holds the files which are open as Terragrunt files, by
// name, whatever their name.
func ParseTerragruntFiles(ctx context.Context, fs ReadOnlyFS, store *state.TerragruntStore, dir string, openLanguages map[string]string) error {
	record, err := store.TerragruntRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid parsing if it is already in progress or already known
	if record.DiagnosticsState[globalAst.HCLParsingSource] != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = store.SetDiagnosticsState(dir, globalAst.HCLParsingSource, op.OpStateLoading)
	if err != nil {
		return err
	}

	files, languages, diags, err := parser.ParseTerragruntFiles(fs, dir, openLanguages)
	if err != nil {
		return err
	}

	sErr := store.UpdateParsedFiles(dir, files, languages, err)
	if sErr != nil {
		return sErr
	}

	return store.UpdateDiagnostics(dir, globalAst.HCLParsingSource, diags)
}
