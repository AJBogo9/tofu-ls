// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ast

import (
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// DiagnosticSource differentiates different sources of diagnostics.
type DiagnosticSource int

const (
	HCLParsingSource DiagnosticSource = iota
	SchemaValidationSource
	ReferenceValidationSource
	TofuValidateSource
	// UnusedSymbolsSource reports variables and locals nothing references.
	UnusedSymbolsSource
	// SemanticValidationSource reports the checks which need the whole
	// module (duplicates, references, types, tfvars, installation).
	SemanticValidationSource
	// FilePathsSource reports file function paths that name no file.
	FilePathsSource
)

func (d DiagnosticSource) String() string {
	return "OpenTofu"
}

type DiagnosticSourceState map[DiagnosticSource]op.OpState

func (dss DiagnosticSourceState) Copy() DiagnosticSourceState {
	newDiagnosticSourceState := make(DiagnosticSourceState, len(dss))
	for source, state := range dss {
		newDiagnosticSourceState[source] = state
	}

	return newDiagnosticSourceState
}
