// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"context"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	tfmod "github.com/opentofu/opentofu-schema/module"
	"github.com/opentofu/opentofu-schema/registry"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/opentofu/tofu-ls/internal/decoder/refcache"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

type StateReader interface {
	DeclaredModuleCalls(modPath string) (map[string]tfmod.DeclaredModuleCall, error)
	LocalModuleMeta(modPath string) (*tfmod.Meta, error)
	ModuleRecordByPath(modPath string) (*state.ModuleRecord, error)
	List() ([]*state.ModuleRecord, error)

	RegistryModuleMeta(addr tfaddr.Module, cons version.Constraints) (*registry.ModuleData, error)
	ProviderSchema(modPath string, addr tfaddr.Provider, vc version.Constraints) (*tfschema.ProviderSchema, error)
}

type RootReader interface {
	InstalledModuleCalls(modPath string) (map[string]tfmod.InstalledModuleCall, error)
	TofuVersion(modPath string) *version.Version
	InstalledModulePath(rootPath string, normalizedSource string) (string, bool)
}

type CombinedReader struct {
	RootReader
	StateReader
}

type PathReader struct {
	RootReader  RootReader
	StateReader StateReader

	// ReferenceCache, when set, keeps the reference contexts of modules
	// across calls of ReferencePathContext.
	ReferenceCache *refcache.Cache[state.ModuleRecord]
}

var _ decoder.PathReader = &PathReader{}
var _ decoder.ReferencePathReader = &PathReader{}

func (pr *PathReader) Paths(ctx context.Context) []lang.Path {
	paths := make([]lang.Path, 0)

	moduleRecords, err := pr.StateReader.List()
	if err != nil {
		return paths
	}

	for _, record := range moduleRecords {
		paths = append(paths, lang.Path{
			Path:       record.Path(),
			LanguageID: ilsp.OpenTofu.String(),
		})
	}

	return paths
}

// PathContext returns a PathContext for the given path based on the language ID.
func (pr *PathReader) PathContext(path lang.Path) (*decoder.PathContext, error) {
	mod, err := pr.StateReader.ModuleRecordByPath(path.Path)
	if err != nil {
		return nil, err
	}
	return modulePathContext(mod, CombinedReader{
		StateReader: pr.StateReader,
		RootReader:  pr.RootReader,
	})
}

// ReferencePathContext returns the reference origins and targets and the
// files of the module, without its schema, for reference lookups across
// modules (see decoder.ReferencePathReader).
func (pr *PathReader) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	mod, err := pr.StateReader.ModuleRecordByPath(path.Path)
	if err != nil {
		pr.ReferenceCache.Forget(path.Path)
		return nil, err
	}
	return pr.ReferenceCache.Get(path.Path, mod, moduleReferenceContext), nil
}
