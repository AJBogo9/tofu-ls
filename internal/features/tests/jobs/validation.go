// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"errors"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	idecoder "github.com/opentofu/tofu-ls/internal/decoder"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/tests/decoder"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	"github.com/opentofu/tofu-ls/internal/job"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// SchemaTestValidation checks the test files and mock data files of a
// directory against their schema: unknown blocks and arguments, label
// counts, repeated blocks and missing required arguments. Provider blocks
// are checked against the module's provider schema when it is known, and
// not at all otherwise.
func SchemaTestValidation(ctx context.Context, testStore *state.TestStore, moduleReader fdecoder.ModuleReader, dir string) error {
	record, err := testStore.TestRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid validation if it is already in progress or already finished
	if record.DiagnosticsState[globalAst.SchemaValidationSource] != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = testStore.SetDiagnosticsState(dir, globalAst.SchemaValidationSource, op.OpStateLoading)
	if err != nil {
		return err
	}

	d := decoder.NewDecoder(&fdecoder.PathReader{
		StateReader:  testStore,
		ModuleReader: moduleReader,
	})
	d.SetContext(idecoder.DecoderContext(ctx))

	diags := make(ast.Diags)
	var rErr error
	for _, languageID := range []string{ilsp.OpenTofuTest.String(), ilsp.OpenTofuMock.String()} {
		pathDecoder, err := d.Path(lang.Path{Path: dir, LanguageID: languageID})
		if err != nil {
			rErr = errors.Join(rErr, err)
			continue
		}
		langDiags, err := pathDecoder.Validate(ctx)
		rErr = errors.Join(rErr, err)
		for name, fileDiags := range langDiags {
			diags[ast.Filename(name)] = fileDiags
		}
	}

	sErr := testStore.UpdateDiagnostics(dir, globalAst.SchemaValidationSource, diags)
	if sErr != nil {
		return sErr
	}

	return rErr
}
