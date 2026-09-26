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
	"bytes"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
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
	// automatically to its parsed content, and each file of VarFiles to
	// its content under the same name.
	VarsFiles map[string]*hcl.File

	// VarsFileErrors maps the name of each of those tfvars files that has
	// a syntax error, or that does not exist, to its first error. OpenTofu
	// refuses to run then.
	VarsFileErrors map[string]string

	// VarFiles are the files given with -var-file, relative to Path and
	// slash-separated, in the order given (see Inputs).
	VarFiles []string

	// EnvVars are the TF_VAR_ environment variables by variable name.
	EnvVars map[string]string

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

// IsOverrideFile reports whether a configuration file is an override file
// (override.tf, *_override.tf and their .tofu and JSON forms), which
// OpenTofu merges into the primary declarations after reading every other
// file.
func IsOverrideFile(name string) bool {
	for _, ext := range []string{".tf.json", ".tofu.json", ".tf", ".tofu"} {
		if strings.HasSuffix(name, ext) {
			base := strings.TrimSuffix(name, ext)
			return base == "override" || strings.HasSuffix(base, "_override")
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
		Path:           dir,
		Files:          make(map[string]*hcl.File),
		VarsFiles:      make(map[string]*hcl.File),
		VarsFileErrors: make(map[string]string),
		Workspace:      "default",
	}
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
		f, diags := parseFile(filepath.Join(dir, name), src)
		if f == nil {
			if isVars {
				mod.VarsFileErrors[name] = diagLine(diags)
			}
			continue
		}
		if isConfig {
			mod.Files[name] = f
		} else {
			mod.VarsFiles[name] = f
			if diags.HasErrors() {
				mod.VarsFileErrors[name] = diagLine(diags)
			}
		}
	}

	// OpenTofu ignores a .tf file when a .tofu file of the same name
	// exists, and likewise for the JSON forms.
	for name := range mod.Files {
		for _, pair := range [][2]string{{".tf.json", ".tofu.json"}, {".tf", ".tofu"}} {
			if strings.HasSuffix(name, pair[0]) {
				if _, ok := mod.Files[strings.TrimSuffix(name, pair[0])+pair[1]]; ok {
					delete(mod.Files, name)
				}
				break
			}
		}
	}

	if src, err := fsys.ReadFile(filepath.Join(dir, ".terraform", "environment")); err == nil {
		if ws := strings.TrimSpace(string(src)); ws != "" {
			mod.Workspace = ws
		}
	}

	if in, ok := fsys.(inputsFS); ok {
		mod.applyInputs(fsys, in.src.Inputs(dir))
	}

	return mod, nil
}

// applyInputs reads the -var-file files of in and keeps its environment
// variables. A file that cannot be read is an error, as in OpenTofu.
func (m *Module) applyInputs(fsys FS, in Inputs) {
	for _, f := range in.VarFiles {
		name, ok := CleanVarFile(f)
		if !ok {
			continue
		}
		m.VarFiles = append(m.VarFiles, name)
		if _, loaded := m.VarsFiles[name]; loaded {
			// an automatically loaded file given again with -var-file
			continue
		}
		src, err := fsys.ReadFile(filepath.Join(m.Path, filepath.FromSlash(name)))
		if err != nil {
			m.VarsFileErrors[name] = "the file does not exist or cannot be read"
			continue
		}
		file, diags := parseFile(filepath.Join(m.Path, filepath.FromSlash(name)), src)
		if file == nil {
			m.VarsFileErrors[name] = diagLine(diags)
			continue
		}
		m.VarsFiles[name] = file
		if diags.HasErrors() {
			m.VarsFileErrors[name] = diagLine(diags)
		}
	}
	m.EnvVars = in.EnvVars
}

// RunAsRoot reports whether a root module shows that OpenTofu runs it as
// it is: it has an automatically loaded tfvars file, -var-file files are
// chosen for it, or it configures a backend. A directory without any of
// these may be a library module (initialized for its examples or tests)
// whose callers set what its defaults leave open.
func (m *Module) RunAsRoot() bool {
	for name := range m.VarsFiles {
		if IsAutoVarsFile(name) && !strings.Contains(name, "/") {
			return true
		}
	}
	if len(m.VarFiles) > 0 {
		return true
	}
	for _, f := range m.Files {
		content, _, _ := f.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}})
		if content == nil {
			continue
		}
		for _, tb := range content.Blocks {
			inner, _, _ := tb.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
				{Type: "backend", LabelNames: []string{"type"}},
				{Type: "cloud"},
			}})
			if inner != nil && len(inner.Blocks) > 0 {
				return true
			}
		}
	}
	return false
}

// IsVarFile reports whether name is one of the module's -var-file files.
func (m *Module) IsVarFile(name string) bool {
	for _, f := range m.VarFiles {
		if f == name {
			return true
		}
	}
	return false
}

// parsedFiles caches the parsed files by path: every hover and inlay
// hint request loads the module and its callers again, and parsing is
// most of that work. An entry is used only while the content is the same.
var parsedFiles = struct {
	sync.Mutex
	files map[string]parsedFile
}{files: make(map[string]parsedFile)}

type parsedFile struct {
	file  *hcl.File
	diags hcl.Diagnostics
}

// maxParsedFiles bounds the cache; it is emptied when full.
const maxParsedFiles = 2048

// parseFile parses a configuration or tfvars file, native or JSON, keeping
// whatever the parser recovered from a file with errors, and the errors.
// The returned file is shared and must not be modified.
func parseFile(path string, src []byte) (*hcl.File, hcl.Diagnostics) {
	parsedFiles.Lock()
	p, ok := parsedFiles.files[path]
	parsedFiles.Unlock()
	if ok && bytes.Equal(p.file.Bytes, src) {
		return p.file, p.diags
	}
	var f *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(path, ".json") {
		f, diags = hcljson.Parse(src, path)
	} else {
		f, diags = hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	}
	if f == nil {
		return nil, diags
	}
	parsedFiles.Lock()
	if len(parsedFiles.files) >= maxParsedFiles {
		parsedFiles.files = make(map[string]parsedFile)
	}
	parsedFiles.files[path] = parsedFile{file: f, diags: diags}
	parsedFiles.Unlock()
	return f, diags
}

// sortedFileNames returns the configuration file names in the order
// OpenTofu merges them in: the primary files in lexical order, then the
// override files in lexical order.
func (m *Module) sortedFileNames() []string {
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, oj := IsOverrideFile(names[i]), IsOverrideFile(names[j])
		if oi != oj {
			return oj
		}
		return names[i] < names[j]
	})
	return names
}

// usedAsRoot reports whether a module directory is run as a root module:
// it has tfvars files that OpenTofu loads automatically, or has been
// initialized.
func usedAsRoot(fsys FS, mod *Module) bool {
	if len(mod.VarsFiles) > 0 {
		return true
	}
	// The language server's filesystem lists a missing directory as
	// empty instead of failing.
	entries, err := fsys.ReadDir(filepath.Join(mod.Path, ".terraform"))
	return err == nil && len(entries) > 0
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
	if _, installed := InstalledRoot(mod.Path); !installed && usedAsRoot(fsys, mod) {
		// A directory with its own tfvars or .terraform is run as a root
		// module, for example a module whose examples/ call it; its
		// values come from its tfvars, whichever callers are indexed.
		return
	}
	callers := LocalCallers(fsys, mod.Path, maxCallerDepth)
	if indexed != nil {
		for _, c := range indexed(mod.Path) {
			if isBelow(c.Parent.Path, mod.Path) {
				// examples/basic calling "../../" runs the module as a
				// child for its example; the module is still a root
				continue
			}
			callers = append(callers, c)
		}
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

// isBelow reports whether dir is a subdirectory of parent.
func isBelow(dir, parent string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(dir))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
