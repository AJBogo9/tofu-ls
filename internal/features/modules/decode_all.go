// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package modules

import (
	"context"

	"github.com/opentofu/tofu-ls/internal/document"
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
