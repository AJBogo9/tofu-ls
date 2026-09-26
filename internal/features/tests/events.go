// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tests

import (
	"context"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	modAst "github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	"github.com/opentofu/tofu-ls/internal/features/tests/jobs"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/protocol"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// discover indexes the test files of a directory the walker found, so
// that Find References from a module lists the uses in its tests.
func (f *TestsFeature) discover(ctx context.Context, path string, files []string) error {
	for _, file := range files {
		if ast.IsTestFilename(file) || ast.IsMockFilename(file) {
			f.logger.Printf("discovered test file in %s", path)

			err := f.store.AddIfNotExists(path)
			if err != nil {
				return err
			}

			_, err = f.decodeTests(ctx, document.DirHandleFromPath(path), false, false)
			return err
		}
	}

	return nil
}

func (f *TestsFeature) didOpen(ctx context.Context, dir document.DirHandle, languageID string) (job.IDs, error) {
	path := dir.Path()

	// Add to state if language ID matches
	if lsp.IsValidTestLanguage(languageID) || lsp.IsValidMockLanguage(languageID) {
		err := f.store.AddIfNotExists(path)
		if err != nil {
			return job.IDs{}, err
		}
	}

	if !f.store.Exists(path) {
		return job.IDs{}, nil
	}

	return f.decodeTests(ctx, dir, false, true)
}

func (f *TestsFeature) didChange(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	if !f.store.Exists(dir.Path()) {
		return job.IDs{}, nil
	}

	return f.decodeTests(ctx, dir, true, true)
}

func (f *TestsFeature) didChangeWatched(ctx context.Context, rawPath string, changeType protocol.FileChangeType, isDir bool) (job.IDs, error) {
	ids := make(job.IDs, 0)

	switch changeType {
	case protocol.Deleted:
		// a directory, or else a file in one
		if f.store.Exists(rawPath) {
			f.removeIndexedTests(rawPath)
			return ids, nil
		}
		parentDir := filepath.Dir(rawPath)
		if !f.store.Exists(parentDir) {
			return ids, nil
		}
		if _, err := os.Stat(parentDir); os.IsNotExist(err) {
			f.removeIndexedTests(parentDir)
			return ids, nil
		}
		return f.decodeTests(ctx, document.DirHandleFromPath(parentDir), true, false)

	case protocol.Changed, protocol.Created:
		dir := document.DirHandleFromPath(filepath.Dir(rawPath))
		if isDir {
			dir = document.DirHandleFromPath(rawPath)
		} else if name := filepath.Base(rawPath); ast.IsTestFilename(name) || ast.IsMockFilename(name) {
			if err := f.store.AddIfNotExists(dir.Path()); err != nil {
				return ids, err
			}
		}
		if !f.store.Exists(dir.Path()) {
			return ids, nil
		}
		return f.decodeTests(ctx, dir, true, false)
	}

	return ids, nil
}

func (f *TestsFeature) removeIndexedTests(rawPath string) {
	dir := document.DirHandleFromPath(rawPath)

	err := f.stateStore.JobStore.DequeueJobsForDir(dir)
	if err != nil {
		f.logger.Printf("failed to dequeue jobs for tests: %s", err)
		return
	}

	err = f.store.Remove(rawPath)
	if err != nil {
		f.logger.Printf("failed to remove tests from state: %s", err)
		return
	}
}

// decodeTests parses the test files of dir and collects their
// references. For open files (open), it also indexes the modules their
// runs run and validates them.
func (f *TestsFeature) decodeTests(ctx context.Context, dir document.DirHandle, ignoreState bool, open bool) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	parseId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseTestFiles(ctx, f.fs, f.store, path)
		},
		Type:        op.OpTypeParseTestFiles.String(),
		IgnoreState: ignoreState,
		Defer: func(ctx context.Context, jobErr error) (job.IDs, error) {
			// also when the files were parsed before (state not changed),
			// e.g. on discovery
			if !open {
				return job.IDs{}, nil
			}
			return f.decodeRunModules(ctx, path)
		},
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, parseId)

	refsId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.DecodeTestReferences(ctx, f.store, path)
		},
		Type:        op.OpTypeDecodeTestReferences.String(),
		DependsOn:   job.IDs{parseId},
		IgnoreState: ignoreState,
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, refsId)

	if !open {
		return ids, nil
	}
	validationOptions, err := lsctx.ValidationOptions(ctx)
	if err != nil {
		// e.g. before initialization: validate on the next change
		return ids, nil
	}
	if validationOptions.EnableEnhancedValidation {
		validationId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
			Dir: dir,
			Func: func(ctx context.Context) error {
				return jobs.SchemaTestValidation(ctx, f.store, f.moduleFeature, path)
			},
			Type:        op.OpTypeSchemaTestValidation.String(),
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

// decodeRunModules indexes the modules which the runs of the test files
// in dir run: the root, where tofu test runs, and the local modules of
// the runs' module blocks.
func (f *TestsFeature) decodeRunModules(ctx context.Context, dir string) (job.IDs, error) {
	ids := make(job.IDs, 0)
	record, err := f.store.TestRecordByPath(dir)
	if err != nil {
		return ids, err
	}

	root := ast.ModuleRoot(dir, f.hasModuleFiles)
	if !f.hasModuleFiles(root) {
		return ids, nil
	}
	dirs := map[string]bool{root: true}
	for name, file := range record.ParsedFiles {
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok || name.IsMock() {
			continue
		}
		for _, run := range ast.Runs(body, root) {
			if run.Module != "" {
				dirs[run.Module] = true
			}
		}
	}

	for modPath := range dirs {
		modIds, err := f.moduleFeature.DecodeLocalModule(ctx, modPath)
		if err != nil {
			f.logger.Printf("indexing module %q of the tests in %q: %s", modPath, dir, err)
			continue
		}
		ids = append(ids, modIds...)
	}
	return ids, nil
}

// hasModuleFiles reports whether dir holds configuration files.
func (f *TestsFeature) hasModuleFiles(dir string) bool {
	entries, err := f.fs.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && modAst.IsModuleFilename(entry.Name()) && !globalAst.IsIgnoredFile(entry.Name()) {
			return true
		}
	}
	return false
}
