// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package lsp

// DiagnosticCode marks (as hcl.Diagnostic.Extra) a diagnostic with a
// stable code, published as the LSP diagnostic's code, and with the data
// a quick fix needs, published as its data.
//
// This is the carrier of the p-complete stream (template-file-missing).
// The p-diag stream owns the shared one; the two are reconciled at merge.
type DiagnosticCode struct {
	Code string
	Data map[string]any
}
