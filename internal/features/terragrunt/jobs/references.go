// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"errors"
	"sort"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	idecoder "github.com/opentofu/tofu-ls/internal/decoder"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/terragrunt/decoder"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/job"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// DecodeTerragruntReferences collects the reference origins and targets
// of the Terragrunt files of a directory: local values through the
// schema, and dependencies, feature flags, units and stacks (see
// decoder.FileTargets). The targets in included files depend on other
// files, so the path context adds them when it is built.
func DecodeTerragruntReferences(ctx context.Context, store *state.TerragruntStore, dir string) error {
	record, err := store.TerragruntRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid collection if it is already in progress or already done
	if record.RefOriginsState != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = store.SetReferencesState(dir, op.OpStateLoading)
	if err != nil {
		return err
	}

	d := decoder.NewDecoder(&fdecoder.PathReader{StateReader: store})
	d.SetContext(idecoder.DecoderContext(ctx))

	origins := make(reference.Origins, 0)
	targets := make(reference.Targets, 0)
	var rErr error
	for _, languageID := range []string{ilsp.Terragrunt.String(), ilsp.TerragruntStack.String()} {
		if !record.HasLanguage(languageID) {
			continue
		}
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

	names := make([]ast.Filename, 0, len(record.ParsedFiles))
	for name := range record.ParsedFiles {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, name := range names {
		targets = append(targets, fdecoder.FileTargets(record.ParsedFiles[name])...)
	}

	sErr := store.UpdateReferences(dir, origins, targets, rErr)
	if sErr != nil {
		return sErr
	}

	return rErr
}
