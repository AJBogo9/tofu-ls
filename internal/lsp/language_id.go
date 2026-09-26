// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

// LanguageID represents the coding language
// of a file
type LanguageID string

const (
	OpenTofu     LanguageID = "opentofu"
	OpenTofuVars LanguageID = "opentofu-vars"
	// OpenTofuTest is the language of test files (*.tftest.hcl, *.tofutest.hcl)
	OpenTofuTest LanguageID = "opentofu-test"
	// OpenTofuMock is the language of mock data files (*.tfmock.hcl)
	OpenTofuMock LanguageID = "opentofu-mock"
	// Terragrunt is the language of terragrunt.hcl and root.hcl
	Terragrunt LanguageID = "terragrunt"
	// TerragruntStack is the language of terragrunt.stack.hcl
	TerragruntStack LanguageID = "terragrunt-stack"
	// Terraform - Some editors do not support language ID overrides which makes it difficult to use this language server
	// We also need to accept language IDs of Terraform to circumvent this issue
	Terraform     LanguageID = "terraform"
	TerraformVars LanguageID = "terraform-vars"
	TerraformTest LanguageID = "terraform-test"
	TerraformMock LanguageID = "terraform-mock"
)

// ParseLanguageID parses a string into a LanguageID
// We also remap Terraform to OpenTofu and TerraformVars to OpenTofuVars
// We assume that the language ID is valid or the validation step has been done before parsing
func ParseLanguageID(id string) LanguageID {
	switch LanguageID(id) {
	case Terraform:
		return OpenTofu
	case TerraformVars:
		return OpenTofuVars
	case TerraformTest:
		return OpenTofuTest
	case TerraformMock:
		return OpenTofuMock
	default:
		return LanguageID(id)
	}
}

func IsValidConfigLanguage(id string) bool {
	switch LanguageID(id) {
	case OpenTofu, Terraform:
		return true
	default:
		return false
	}
}

func IsValidVarsLanguage(id string) bool {
	switch LanguageID(id) {
	case OpenTofuVars, TerraformVars:
		return true
	default:
		return false
	}
}

func IsValidTestLanguage(id string) bool {
	switch LanguageID(id) {
	case OpenTofuTest, TerraformTest:
		return true
	default:
		return false
	}
}

func IsValidMockLanguage(id string) bool {
	switch LanguageID(id) {
	case OpenTofuMock, TerraformMock:
		return true
	default:
		return false
	}
}

// IsValidTerragruntLanguage reports whether the language is one of the
// Terragrunt configuration files.
func IsValidTerragruntLanguage(id string) bool {
	switch LanguageID(id) {
	case Terragrunt, TerragruntStack:
		return true
	default:
		return false
	}
}

func (l LanguageID) String() string {
	return string(l)
}
