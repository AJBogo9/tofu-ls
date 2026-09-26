// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/tests/parser"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	"github.com/opentofu/tofu-ls/internal/job"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// ParseTestFiles parses the test files and mock data files of a
// directory, i.e. turns the bytes of *.tftest.hcl, *.tofutest.hcl and
// *.tfmock.hcl files into AST ([*hcl.File]). Test files are small and
// few, so every file is parsed again on any change.
func ParseTestFiles(ctx context.Context, fs ReadOnlyFS, testStore *state.TestStore, dir string) error {
	record, err := testStore.TestRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid parsing if it is already in progress or already known
	if record.DiagnosticsState[globalAst.HCLParsingSource] != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = testStore.SetDiagnosticsState(dir, globalAst.HCLParsingSource, op.OpStateLoading)
	if err != nil {
		return err
	}

	files, diags, err := parser.ParseTestFiles(fs, dir)
	if err != nil {
		return err
	}

	sErr := testStore.UpdateParsedFiles(dir, files, err)
	if sErr != nil {
		return sErr
	}

	return testStore.UpdateDiagnostics(dir, globalAst.HCLParsingSource, diags)
}
