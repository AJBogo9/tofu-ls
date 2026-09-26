// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package modules

import (
	"context"
	"path/filepath"

	tfmod "github.com/opentofu/opentofu-schema/module"
	tfaddr "github.com/opentofu/registry-address"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/modules/jobs"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/job"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// DecodeAllModules schedules decoding of every discovered module whose
// references are not known yet, such as the parent of the only open
// module, so that references and rename see every use. It returns the
// IDs of the scheduled jobs to wait for.
//
// Modules found by the walker are otherwise only decoded once one of
// their files is opened.
func (f *ModulesFeature) DecodeAllModules(ctx context.Context) (job.IDs, error) {
	ids := make(job.IDs, 0)

	records, err := f.Store.List()
	if err != nil {
		return ids, err
	}

	for _, mod := range records {
		if mod.RefOriginsState == op.OpStateLoaded {
			continue
		}
		// not first level: no module call decoding or registry requests
		modIds, err := f.decodeModule(ctx, document.DirHandleFromPath(mod.Path()), false, false)
		if err != nil {
			f.logger.Printf("decoding module %q for workspace references failed: %s", mod.Path(), err)
			continue
		}
		ids = append(ids, modIds...)
	}

	return ids, nil
}

// DecodeCallersOf decodes the modules which call the module at modPath
// and whose references are not known yet, so that counting the uses of
// its declarations sees the uses in its callers, as Find References does
// after DecodeAllModules, without decoding the rest of the workspace.
// Only callers can refer to a module's variables and outputs.
//
// Finding the callers needs the module calls of every module, so modules
// that were only discovered are parsed and their metadata loaded first,
// which is cheap next to decoding them. It returns once the jobs are done
// or ctx is done.
func (f *ModulesFeature) DecodeCallersOf(ctx context.Context, modPath string) error {
	records, err := f.Store.List()
	if err != nil {
		return err
	}
	ids := make(job.IDs, 0)
	for _, mod := range records {
		if mod.MetaState != op.OpStateUnknown || mod.RefOriginsState == op.OpStateLoaded {
			continue
		}
		metaIds, err := f.loadModuleMetadata(ctx, document.DirHandleFromPath(mod.Path()))
		if err != nil {
			f.logger.Printf("loading metadata of %q to find the callers of %q failed: %s", mod.Path(), modPath, err)
			continue
		}
		ids = append(ids, metaIds...)
	}
	if err := f.stateStore.JobStore.WaitForJobs(ctx, ids...); err != nil {
		return err
	}

	records, err = f.Store.List()
	if err != nil {
		return err
	}
	ids = make(job.IDs, 0)
	for _, mod := range records {
		if mod.RefOriginsState == op.OpStateLoaded || mod.Path() == modPath || !f.callsModule(mod, modPath) {
			continue
		}
		// not first level: no module call decoding or registry requests
		modIds, err := f.decodeModule(ctx, document.DirHandleFromPath(mod.Path()), false, false)
		if err != nil {
			f.logger.Printf("decoding %q, a caller of %q, failed: %s", mod.Path(), modPath, err)
			continue
		}
		ids = append(ids, modIds...)
	}
	return f.stateStore.JobStore.WaitForJobs(ctx, ids...)
}

// loadModuleMetadata parses a module and loads its metadata, the first
// two jobs of decodeModule, which schedules the rest once they are done
// and so skips them when they ran here.
func (f *ModulesFeature) loadModuleMetadata(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	path := dir.Path()
	parseValidationOptions, _ := lsctx.ValidationOptions(ctx)

	parseId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			err := jobs.ParseModuleConfiguration(ctx, f.fs, f.Store, path)
			if err == nil && parseValidationOptions.UnusedSymbols {
				// as in decodeModule, which will not parse again
				return jobs.UnusedSymbols(ctx, f.fs, f.Store, path)
			}
			return err
		},
		Type: op.OpTypeParseModuleConfiguration.String(),
	})
	if err != nil {
		return nil, err
	}

	metaId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.LoadModuleMetadata(ctx, f.Store, path)
		},
		Type:      op.OpTypeLoadModuleMetadata.String(),
		DependsOn: job.IDs{parseId},
	})
	if err != nil {
		return job.IDs{parseId}, err
	}

	return job.IDs{parseId, metaId}, nil
}

// callsModule tells whether mod has a module call whose source is the
// module at modPath, resolved as the schema of the call resolves it: a
// local path, or the directory tofu init installed the source into.
func (f *ModulesFeature) callsModule(mod *state.ModuleRecord, modPath string) bool {
	for _, call := range mod.Meta.ModuleCalls {
		switch src := call.SourceAddr.(type) {
		case tfmod.LocalSourceAddr:
			if filepath.Join(mod.Path(), src.String()) == modPath {
				return true
			}
		case tfaddr.Module, tfmod.RemoteSourceAddr:
			if f.rootFeature == nil {
				continue
			}
			dir, ok := f.rootFeature.InstalledModulePath(mod.Path(), src.String())
			if ok && filepath.Join(mod.Path(), dir) == modPath {
				return true
			}
		}
	}
	return false
}
