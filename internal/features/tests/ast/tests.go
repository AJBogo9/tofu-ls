// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package ast

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// Filename is the name of a test file (*.tftest.hcl, *.tofutest.hcl) or
// of a mock data file (*.tfmock.hcl) within its directory.
type Filename string

// IsTestFilename reports whether tofu test reads the file as a test file.
func IsTestFilename(name string) bool {
	return strings.HasSuffix(name, ".tftest.hcl") ||
		strings.HasSuffix(name, ".tofutest.hcl")
}

// IsMockFilename reports whether the file is a mock data file, which
// Terraform reads and OpenTofu 1.12 does not.
func IsMockFilename(name string) bool {
	return strings.HasSuffix(name, ".tfmock.hcl")
}

func (f Filename) String() string {
	return string(f)
}

func (f Filename) IsMock() bool {
	return IsMockFilename(string(f))
}

// IsJSON is false: tofu test also reads .tftest.json files, which the
// language server leaves alone.
func (f Filename) IsJSON() bool {
	return false
}

type Files map[Filename]*hcl.File

func (f Files) Copy() Files {
	m := make(Files, len(f))
	for name, file := range f {
		m[name] = file
	}
	return m
}

type Diags map[Filename]hcl.Diagnostics

func DiagsFromMap(m map[string]hcl.Diagnostics) Diags {
	d := make(Diags, len(m))
	for name, diags := range m {
		d[Filename(name)] = diags
	}
	return d
}

func (d Diags) Copy() Diags {
	m := make(Diags, len(d))
	for name, diags := range d {
		m[name] = diags
	}
	return m
}

func (d Diags) AsMap() map[string]hcl.Diagnostics {
	m := make(map[string]hcl.Diagnostics, len(d))
	for name, diags := range d {
		m[string(name)] = diags
	}
	return m
}

func (d Diags) Count() int {
	count := 0
	for _, diags := range d {
		count += len(diags)
	}
	return count
}

type SourceDiags map[globalAst.DiagnosticSource]Diags

func (sd SourceDiags) Count() int {
	count := 0
	for _, diags := range sd {
		count += diags.Count()
	}
	return count
}
