// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl-lang/schema"
	tfmodule "github.com/opentofu/opentofu-schema/module"
	"github.com/opentofu/opentofu-schema/registry"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
)

// schemaForModule returns the schema of the module, and the names of the
// module calls whose inputs come from the registry's data about their
// module (not installed, or not indexed yet) instead of the module.
func schemaForModule(mod *state.ModuleRecord, stateReader CombinedReader) (*schema.BodySchema, map[string]bool, error) {
	resolvedVersion := tfschema.ResolveVersion(stateReader.TofuVersion(mod.Path()), mod.Meta.CoreRequirements)
	sm := tfschema.NewSchemaMerger(mustCoreSchemaForVersion(resolvedVersion))
	sm.SetTofuVersion(resolvedVersion)
	recorder := &registryRecorder{CombinedReader: stateReader, sources: make(map[string]bool)}
	sm.SetStateReader(recorder)

	meta := &tfmodule.Meta{
		Path:                 mod.Path(),
		CoreRequirements:     mod.Meta.CoreRequirements,
		ProviderRequirements: mod.Meta.ProviderRequirements,
		ProviderReferences:   mod.Meta.ProviderReferences,
		Variables:            mod.Meta.Variables,
		Filenames:            mod.Meta.Filenames,
		ModuleCalls:          mod.Meta.ModuleCalls,
	}

	bodySchema, err := sm.SchemaForModule(meta)
	if err != nil {
		return nil, nil, err
	}
	var registryCalls map[string]bool
	for name, mc := range mod.Meta.ModuleCalls {
		if addr, ok := mc.SourceAddr.(tfaddr.Module); ok && recorder.sources[addr.String()] {
			if registryCalls == nil {
				registryCalls = make(map[string]bool)
			}
			registryCalls[name] = true
		}
	}
	return bodySchema, registryCalls, nil
}

// registryRecorder records the module sources whose schema the schema
// merger builds from registry data.
type registryRecorder struct {
	CombinedReader
	sources map[string]bool
}

func (r *registryRecorder) RegistryModuleMeta(addr tfaddr.Module, cons version.Constraints) (*registry.ModuleData, error) {
	data, err := r.CombinedReader.RegistryModuleMeta(addr, cons)
	if err == nil {
		r.sources[addr.String()] = true
	}
	return data, err
}

func mustCoreSchemaForVersion(v *version.Version) *schema.BodySchema {
	s, err := tfschema.CoreModuleSchemaForVersion(v)
	if err != nil {
		// this should never happen
		panic(err)
	}
	s = tfschema.WithMetaArgumentModifiers(s)
	return s
}
