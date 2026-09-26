// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package terragrunt

import (
	"context"
	"os"
	"path/filepath"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/jobs"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/protocol"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

func (f *TerragruntFeature) didOpen(ctx context.Context, dir document.DirHandle, languageID string) (job.IDs, error) {
	path := dir.Path()

	// Add to state if language ID matches
	if lsp.IsValidTerragruntLanguage(languageID) {
		err := f.store.AddIfNotExists(path)
		if err != nil {
			return job.IDs{}, err
		}
	}

	if !f.store.Exists(path) {
		return job.IDs{}, nil
	}

	// a file opened with another language may leave the directory's
	// Terragrunt files, so they are parsed again
	return f.decodeTerragrunt(ctx, dir, true)
}

func (f *TerragruntFeature) didChange(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	if !f.store.Exists(dir.Path()) {
		return job.IDs{}, nil
	}

	return f.decodeTerragrunt(ctx, dir, true)
}

func (f *TerragruntFeature) didChangeWatched(ctx context.Context, rawPath string, changeType protocol.FileChangeType, isDir bool) (job.IDs, error) {
	ids := make(job.IDs, 0)

	switch changeType {
	case protocol.Deleted:
		// a directory, or else a file in one
		if f.store.Exists(rawPath) {
			f.removeIndexedTerragrunt(rawPath)
			return ids, nil
		}
		parentDir := filepath.Dir(rawPath)
		if !f.store.Exists(parentDir) {
			return ids, nil
		}
		if _, err := os.Stat(parentDir); os.IsNotExist(err) {
			f.removeIndexedTerragrunt(parentDir)
			return ids, nil
		}
		return f.decodeTerragrunt(ctx, document.DirHandleFromPath(parentDir), true)

	case protocol.Changed, protocol.Created:
		dir := document.DirHandleFromPath(filepath.Dir(rawPath))
		if isDir {
			dir = document.DirHandleFromPath(rawPath)
		}
		if !f.store.Exists(dir.Path()) {
			return ids, nil
		}
		return f.decodeTerragrunt(ctx, dir, true)
	}

	return ids, nil
}

func (f *TerragruntFeature) removeIndexedTerragrunt(rawPath string) {
	dir := document.DirHandleFromPath(rawPath)

	err := f.stateStore.JobStore.DequeueJobsForDir(dir)
	if err != nil {
		f.logger.Printf("failed to dequeue jobs for terragrunt: %s", err)
		return
	}

	err = f.store.Remove(rawPath)
	if err != nil {
		f.logger.Printf("failed to remove terragrunt from state: %s", err)
		return
	}
}

// openLanguages returns the files of dir which are open as Terragrunt
// files, with their language, by name.
func (f *TerragruntFeature) openLanguages(dir document.DirHandle) map[string]string {
	languages := make(map[string]string)
	docs, err := f.stateStore.DocumentStore.ListDocumentsInDir(dir)
	if err != nil {
		return languages
	}
	for _, doc := range docs {
		if lsp.IsValidTerragruntLanguage(doc.LanguageID) {
			languages[doc.Filename] = doc.LanguageID
		} else if ast.LanguageOfFilename(doc.Filename) != "" {
			// open with another language: not a Terragrunt file then
			languages[doc.Filename] = ""
		}
	}
	return languages
}

// decodeTerragrunt parses the Terragrunt files of dir, collects their
// references and checks them against the schema.
func (f *TerragruntFeature) decodeTerragrunt(ctx context.Context, dir document.DirHandle, ignoreState bool) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	parseId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseTerragruntFiles(ctx, f.fs, f.store, path, f.openLanguages(dir))
		},
		Type:        op.OpTypeParseTerragruntFiles.String(),
		IgnoreState: ignoreState,
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, parseId)

	refsId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.DecodeTerragruntReferences(ctx, f.store, path)
		},
		Type:        op.OpTypeDecodeTerragruntReferences.String(),
		DependsOn:   job.IDs{parseId},
		IgnoreState: ignoreState,
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, refsId)

	validationOptions, err := lsctx.ValidationOptions(ctx)
	if err != nil {
		// e.g. before initialization: validate on the next change
		return ids, nil
	}
	if validationOptions.EnableEnhancedValidation && validationOptions.Terragrunt {
		validationId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
			Dir: dir,
			Func: func(ctx context.Context) error {
				return jobs.SchemaTerragruntValidation(ctx, f.store, path)
			},
			Type:        op.OpTypeSchemaTerragruntValidation.String(),
			DependsOn:   job.IDs{parseId},
			IgnoreState: ignoreState,
		})
		if err != nil {
			return ids, err
		}
		ids = append(ids, validationId)
	}

	return ids, nil
}
