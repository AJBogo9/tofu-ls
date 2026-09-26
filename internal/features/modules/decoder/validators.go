// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"github.com/hashicorp/hcl-lang/validator"
	"github.com/opentofu/tofu-ls/internal/features/modules/decoder/validations"
)

var moduleValidators = []validator.Validator{
	validator.BlockLabelsLength{},
	validator.DeprecatedAttribute{},
	validator.DeprecatedBlock{},
	validator.MaxBlocks{},
	validator.MinBlocks{},
	validations.MissingRequiredAttribute{},
	validator.UnexpectedAttribute{},
	validator.UnexpectedBlock{},
}

// validatorsForModule returns the validators of a module whose calls
// named in registryCalls have their inputs from the registry's data.
func validatorsForModule(registryCalls map[string]bool) []validator.Validator {
	if len(registryCalls) == 0 {
		return moduleValidators
	}
	return append([]validator.Validator{validations.RegistryModuleInputs{Calls: registryCalls}}, moduleValidators...)
}
