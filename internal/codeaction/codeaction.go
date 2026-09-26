// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package codeaction computes quick fixes for the server's diagnostics and
// cursor-based rewrites (intentions). Every action is a complete edit that
// leaves valid configuration; an action that cannot be made safe is not
// offered.
//
// Everything here works on source text with byte offsets, so that the
// actions can be tested without a running server.
package codeaction

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// Kinds of the actions, as LSP code action kinds.
const (
	KindQuickFix        = "quickfix"
	KindRefactorRewrite = "refactor.rewrite"
)

// Diagnostic codes the quick fixes answer, as the diagnostics publish
// them (see the Code constants in internal/lsp).
const (
	CodeUnusedVariable           = ilsp.CodeUnusedVariable
	CodeUnusedLocal              = ilsp.CodeUnusedLocal
	CodeUnusedDataSource         = ilsp.CodeUnusedDataSource
	CodeUnresolvedReference      = ilsp.CodeUnresolvedReference
	CodeTfvarsUndeclaredVariable = ilsp.CodeTfvarsUndeclared
	CodeMissingRequiredAttribute = ilsp.CodeMissingRequiredAttribute
	CodeModuleNotInstalled       = ilsp.CodeModuleNotInstalled
	CodeProviderNotInstalled     = ilsp.CodeProviderNotInstalled
	CodeInterpolationOnly        = ilsp.CodeInterpolationOnly
)

// Edit replaces Range of the file at the absolute path File with NewText.
// The range holds correct lines and byte offsets of the file's current
// text; columns count bytes.
type Edit struct {
	File    string
	Range   hcl.Range
	NewText string
}

// Command is a client command an action runs after its edits.
type Command struct {
	Title     string
	Name      string
	Arguments []interface{}
}

// Action is one quick fix or intention.
type Action struct {
	Title     string
	Kind      string
	Preferred bool
	// Disabled, when not empty, says why the action cannot be applied.
	Disabled string
	Edits    []Edit
	// Create lists the files (absolute paths) the edits create.
	Create  []string
	Command *Command
}

// Document is the file an action is requested for.
type Document struct {
	// Path is the absolute path of the file.
	Path string
	Text []byte
	// Vars tells whether the file is a variable definitions (.tfvars) file.
	Vars bool
}

func (d Document) dir() string {
	return filepath.Dir(d.Path)
}

// Diagnostic is a diagnostic of the document, as the client sends it back.
type Diagnostic struct {
	Code string
	// Data is the diagnostic's data object, or nil.
	Data map[string]interface{}
	// Range has correct lines and byte offsets in the document.
	Range hcl.Range
}

func (d Diagnostic) dataString(key string) string {
	if d.Data == nil {
		return ""
	}
	s, _ := d.Data[key].(string)
	return s
}

func (d Diagnostic) dataStrings(key string) []string {
	if d.Data == nil {
		return nil
	}
	list, _ := d.Data[key].([]interface{})
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Location is a range in a file (an absolute path).
type Location struct {
	File  string
	Range hcl.Range
}

// Env gives the actions access to the configuration around the document.
// Every function may be nil.
type Env struct {
	// ReadFile reads a file by absolute path, preferring the open document.
	ReadFile func(path string) ([]byte, error)
	// ReadDir lists a directory.
	ReadDir func(dir string) ([]fs.DirEntry, error)
	// Schema returns the body schema of the module in dir, or nil.
	Schema func(dir string) *schema.BodySchema
	// VariableType returns the declared type of a variable of the module
	// in dir; false when the variable is unknown or declares no type.
	VariableType func(dir, name string) (cty.Type, bool)
	// VariableSettings returns where a variable of the module in dir is
	// set outside the module: keys of var files, keys of variables blocks
	// in test files and arguments of module calls. Each range is what
	// deleting the setting removes. It fails when a file which could set
	// the variable cannot be read.
	VariableSettings func(dir, name string) ([]Location, bool)
	// CreateFiles tells whether the client can create files as part of
	// an edit.
	CreateFiles bool
	// InitCommand is the client command which runs tofu init for a
	// module directory given as a URI, or "" when the client has none.
	InitCommand string
	// DirURI turns a directory into the URI the init command takes.
	DirURI func(dir string) string
	// ParsedFile returns the syntax tree the server already holds for the
	// file at path, when it was parsed from src without errors, which
	// saves parsing it again on every request.
	ParsedFile func(path string, src []byte) (*hclsyntax.Body, bool)
}

// parse parses a native syntax file, or takes the server's tree of it.
func (env Env) parse(path string, src []byte) (*hclsyntax.Body, bool) {
	if env.ParsedFile != nil {
		if body, ok := env.ParsedFile(path, src); ok {
			return body, true
		}
	}
	return parse(path, src)
}

func (env Env) readFile(path string) ([]byte, bool) {
	if env.ReadFile == nil {
		return nil, false
	}
	src, err := env.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return src, true
}

// moduleFiles returns the absolute paths of the native syntax
// configuration files (.tf, .tofu) of the module in dir, sorted.
func (env Env) moduleFiles(dir string) []string {
	if env.ReadDir == nil {
		return nil
	}
	entries, err := env.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := make([]string, 0)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tofu") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	return files
}

// QuickFixes returns the fixes for one diagnostic of the document.
func QuickFixes(env Env, doc Document, diag Diagnostic) []Action {
	var actions []Action
	switch diag.Code {
	case CodeUnusedVariable:
		actions = removeUnusedVariable(env, doc, diag)
	case CodeUnusedLocal:
		actions = removeUnusedLocal(env, doc, diag)
	case CodeUnusedDataSource:
		actions = removeUnusedDataSource(env, doc, diag)
	case CodeUnresolvedReference:
		actions = declareMissing(env, doc, diag)
	case CodeTfvarsUndeclaredVariable:
		actions = declareTfvarsVariable(env, doc, diag)
	case CodeMissingRequiredAttribute:
		actions = addRequiredArguments(env, doc, diag)
	case CodeModuleNotInstalled, CodeProviderNotInstalled:
		actions = runInit(env, doc, diag)
	case CodeInterpolationOnly:
		actions = unwrapInterpolation(env, doc, diag)
	}
	for i := range actions {
		actions[i].Kind = KindQuickFix
	}
	return actions
}

// Intentions returns the rewrites offered at pos, a position (with a
// correct byte offset) in the document.
func Intentions(env Env, doc Document, pos hcl.Pos) []Action {
	if doc.Vars {
		return nil
	}
	f, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return nil
	}
	var actions []Action
	actions = append(actions, rewriteCall(env, doc, f, pos)...)
	actions = append(actions, addRequiredProvider(env, doc, f, pos)...)
	for i := range actions {
		actions[i].Kind = KindRefactorRewrite
	}
	return actions
}
