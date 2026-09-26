// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"errors"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	idecoder "github.com/opentofu/tofu-ls/internal/decoder"
	"github.com/opentofu/tofu-ls/internal/document"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/tests/decoder"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	"github.com/opentofu/tofu-ls/internal/job"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// DecodeTestReferences collects the reference origins and targets of the
// test files and mock data files of a directory with the static schema.
// The origins are stored as they are found; the path context resolves
// them against the module which each run runs, which may change later.
func DecodeTestReferences(ctx context.Context, testStore *state.TestStore, dir string) error {
	record, err := testStore.TestRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid collection if it is already in progress or already done
	if record.RefOriginsState != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = testStore.SetReferencesState(dir, op.OpStateLoading)
	if err != nil {
		return err
	}

	d := decoder.NewDecoder(&fdecoder.PathReader{
		StateReader:     testStore,
		UseStaticSchema: true,
	})
	d.SetContext(idecoder.DecoderContext(ctx))

	origins := make(reference.Origins, 0)
	targets := make(reference.Targets, 0)
	var rErr error
	for _, languageID := range []string{ilsp.OpenTofuTest.String(), ilsp.OpenTofuMock.String()} {
		pathDecoder, err := d.Path(lang.Path{Path: dir, LanguageID: languageID})
		if err != nil {
			rErr = errors.Join(rErr, err)
			continue
		}
		o, err := pathDecoder.CollectReferenceOrigins()
		rErr = errors.Join(rErr, err)
		origins = append(origins, o...)
		t, err := pathDecoder.CollectReferenceTargets()
		rErr = errors.Join(rErr, err)
		targets = append(targets, t...)
	}

	sErr := testStore.UpdateReferences(dir, origins, targets, rErr)
	if sErr != nil {
		return sErr
	}

	return rErr
}
