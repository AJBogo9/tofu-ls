// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package ast

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/lsp"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// The names of Terragrunt configuration files. root.hcl is Terragrunt's
// recommended name for the configuration which units include; any other
// .hcl file is Terragrunt's only when the editor opens it as such (e.g.
// through files.associations).
const (
	UnitFilename  = "terragrunt.hcl"
	RootFilename  = "root.hcl"
	StackFilename = "terragrunt.stack.hcl"
)

// LanguageOfFilename returns the language of a Terragrunt file by its
// name, or "" for any other file.
func LanguageOfFilename(name string) string {
	switch name {
	case UnitFilename, RootFilename:
		return lsp.Terragrunt.String()
	case StackFilename:
		return lsp.TerragruntStack.String()
	}
	return ""
}

// IsEntryFilename reports whether Terragrunt starts from the file (a
// unit or a stack), as opposed to a file that another one includes or
// reads. Functions such as find_in_parent_folders() and relative paths
// in included files depend on the file which includes them, so they are
// only resolved in entry files.
func IsEntryFilename(name string) bool {
	return name == UnitFilename || name == StackFilename
}

// Filename is the name of a Terragrunt file within its directory.
type Filename string

func (f Filename) String() string {
	return string(f)
}

// IsJSON is false: terragrunt.hcl.json files are left alone.
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

// Languages holds the language (terragrunt or terragrunt-stack) of each
// file.
type Languages map[Filename]string

func (l Languages) Copy() Languages {
	m := make(Languages, len(l))
	for name, lang := range l {
		m[name] = lang
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
