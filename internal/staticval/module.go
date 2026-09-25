// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package staticval evaluates what can be known about a module's values
// without a plan: input variables from tfvars files and defaults, locals
// evaluated with the pure built-in functions, and for_each/count
// collections. Everything that depends on the plan (resource and data
// attributes, module outputs, impure functions) stays unknown, together
// with the reason why.
package staticval

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

// FS is the subset of the language server's filesystem needed to load a
// module. It sees unsaved editor buffers.
type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
}

// Module holds the parsed files of one module directory.
type Module struct {
	// Path is the absolute directory of the module.
	Path string

	// Files maps the base name of each configuration file (.tf, .tofu,
	// .tf.json, .tofu.json) to its parsed content.
	Files map[string]*hcl.File

	// VarsFiles maps the base name of each tfvars file that OpenTofu loads
	// automatically to its parsed content.
	VarsFiles map[string]*hcl.File

	// Workspace is the selected workspace, "default" when none is known.
	Workspace string

	// RootPath is the root module's directory when this module is known to
	// be called from it. It is empty for a root module.
	RootPath string

	// Callers are the known module calls of this module.
	Callers []Caller
}

// Caller is a module block in another module that calls this module.
type Caller struct {
	Parent *Module
	Name   string
}

// IsConfigFile reports whether a file name is a configuration file of a
// module, ignoring editor backup files like the language server does.
func IsConfigFile(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") || strings.HasSuffix(name, "~") {
		return false
	}
	for _, ext := range []string{".tf", ".tofu", ".tf.json", ".tofu.json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// IsAutoVarsFile reports whether OpenTofu loads a tfvars file without a
// -var-file flag.
func IsAutoVarsFile(name string) bool {
	switch name {
	case "terraform.tfvars", "terraform.tfvars.json":
		return true
	}
	return strings.HasSuffix(name, ".auto.tfvars") || strings.HasSuffix(name, ".auto.tfvars.json")
}

// VarsFileOrder returns the tfvars file names in the order OpenTofu
// applies them. Later files override earlier ones: terraform.tfvars, then
// terraform.tfvars.json, then the *.auto.tfvars and *.auto.tfvars.json
// files in lexical order.
func VarsFileOrder(names []string) []string {
	var auto []string
	var ordered []string
	for _, fixed := range []string{"terraform.tfvars", "terraform.tfvars.json"} {
		for _, n := range names {
			if n == fixed {
				ordered = append(ordered, n)
			}
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".auto.tfvars") || strings.HasSuffix(n, ".auto.tfvars.json") {
			auto = append(auto, n)
		}
	}
	sort.Strings(auto)
	return append(ordered, auto...)
}

// LoadModule reads and parses the configuration and tfvars files of dir.
// Files that fail to parse completely are kept with whatever the parser
// recovered.
func LoadModule(fsys FS, dir string) (*Module, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	mod := &Module{
		Path:      dir,
		Files:     make(map[string]*hcl.File),
		VarsFiles: make(map[string]*hcl.File),
		Workspace: "default",
	}
	parser := hclparse.NewParser()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isConfig := IsConfigFile(name)
		isVars := IsAutoVarsFile(name)
		if !isConfig && !isVars {
			continue
		}
		src, err := fsys.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var f *hcl.File
		if strings.HasSuffix(name, ".json") {
			f, _ = parser.ParseJSON(src, filepath.Join(dir, name))
		} else {
			f, _ = parser.ParseHCL(src, filepath.Join(dir, name))
		}
		if f == nil {
			continue
		}
		if isConfig {
			mod.Files[name] = f
		} else {
			mod.VarsFiles[name] = f
		}
	}

	if src, err := fsys.ReadFile(filepath.Join(dir, ".terraform", "environment")); err == nil {
		if ws := strings.TrimSpace(string(src)); ws != "" {
			mod.Workspace = ws
		}
	}

	return mod, nil
}

// sortedFileNames returns the configuration file names in lexical order,
// which is the order OpenTofu merges them in.
func (m *Module) sortedFileNames() []string {
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// maxCallerDepth is how many directories up AddCallers looks for module
// blocks that call a module.
const maxCallerDepth = 4

// AddCallers records the modules that call mod through a local source:
// those that indexed returns (the language server's index, may be nil)
// and those in ancestor directories, which may not be indexed yet when
// only the child's files are open. Callers get their own callers up to
// nesting levels, so a caller that is itself a child module takes its
// values from its callers instead of its defaults. A module installed
// under .terraform/modules counts as a child even when no caller is found.
func AddCallers(fsys FS, mod *Module, nesting int, indexed func(dir string) []Caller) {
	defer func() {
		if root, ok := InstalledRoot(mod.Path); ok && mod.RootPath == "" {
			mod.RootPath = root
		}
	}()
	callers := LocalCallers(fsys, mod.Path, maxCallerDepth)
	if indexed != nil {
		callers = append(callers, indexed(mod.Path)...)
	}
	if nesting <= 0 {
		// Too deep to resolve the callers, but a called module is still a
		// child: its variables stay unknown instead of taking defaults.
		if len(callers) > 0 && mod.RootPath == "" {
			mod.RootPath = callers[0].Parent.Path
		}
		return
	}

	seen := make(map[string]bool)
	resolved := make(map[*Module]bool)
	for _, c := range callers {
		key := filepath.Clean(c.Parent.Path) + "\x00" + c.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		if !resolved[c.Parent] {
			resolved[c.Parent] = true
			AddCallers(fsys, c.Parent, nesting-1, indexed)
		}
		mod.Callers = append(mod.Callers, c)
	}
	sort.Slice(mod.Callers, func(i, j int) bool {
		if mod.Callers[i].Parent.Path != mod.Callers[j].Parent.Path {
			return mod.Callers[i].Parent.Path < mod.Callers[j].Parent.Path
		}
		return mod.Callers[i].Name < mod.Callers[j].Name
	})
	if mod.RootPath == "" && len(mod.Callers) > 0 {
		parent := mod.Callers[0].Parent
		mod.RootPath = parent.Path
		if parent.RootPath != "" {
			mod.RootPath = parent.RootPath
		}
	}
}

// LocalCallers returns the module blocks of dir's ancestor directories (up
// to maxDepth levels up) whose literal local source points at dir. It
// needs no index, so a child module's values come from its callers even
// when only the child's files are open.
func LocalCallers(fsys FS, dir string, maxDepth int) []Caller {
	var callers []Caller
	dir = filepath.Clean(dir)
	parentDir := dir
	for i := 0; i < maxDepth; i++ {
		next := filepath.Dir(parentDir)
		if next == parentDir {
			break
		}
		parentDir = next
		if filepath.Base(parentDir) == ".terraform" {
			break
		}
		parent, err := LoadModule(fsys, parentDir)
		if err != nil || len(parent.Files) == 0 {
			continue
		}
		for _, name := range parent.moduleCallsTo(dir) {
			callers = append(callers, Caller{Parent: parent, Name: name})
		}
	}
	return callers
}

// moduleCallsTo returns the names of the module blocks whose literal local
// source resolves to dir.
func (m *Module) moduleCallsTo(dir string) []string {
	var names []string
	for _, fname := range m.sortedFileNames() {
		content, _, _ := m.Files[fname].Body.PartialContent(&hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}},
		})
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			attrs, _ := block.Body.JustAttributes()
			src, ok := attrs["source"]
			if !ok {
				continue
			}
			val, diags := src.Expr.Value(nil)
			if diags.HasErrors() || val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
				continue
			}
			s := val.AsString()
			if !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
				continue
			}
			if filepath.Clean(filepath.Join(m.Path, s)) == dir {
				names = append(names, block.Labels[0])
			}
		}
	}
	return names
}

// InstalledRoot returns the root module directory of a module installed
// under .terraform/modules, which is always called by that root.
func InstalledRoot(dir string) (string, bool) {
	sep := string(filepath.Separator)
	marker := sep + ".terraform" + sep + "modules" + sep
	i := strings.Index(dir, marker)
	if i < 0 {
		return "", false
	}
	return dir[:i], true
}
