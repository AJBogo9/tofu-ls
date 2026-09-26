// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"github.com/hashicorp/hcl-lang/validator"
)

// Names which no variable declares are reported by the module's semantic
// validation (tfvars-undeclared-variable), which runs after the module's
// variables are known; here they depended on which job ran first.
var varsValidators = []validator.Validator{
	validator.UnexpectedBlock{},
}
