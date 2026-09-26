// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package paths works out, without running Terragrunt, the files and
// directories that the paths of a Terragrunt file name: included files,
// dependencies, local sources and files read by functions.
//
// Only what is certain is resolved. Expressions are evaluated with
// string literals, the local values that are themselves static, and a
// few functions that are implemented exactly as Terragrunt does:
// find_in_parent_folders, get_terragrunt_dir, get_original_terragrunt_dir,
// get_repo_root and some string functions. Functions that depend on the
// including file are only available in entry files (a unit's
// terragrunt.hcl or a terragrunt.stack.hcl), where they evaluate to the
// file's own directory.
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/filepaths"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error)
}

// maxFoldersToCheck is Terragrunt's default limit of find_in_parent_folders.
const maxFoldersToCheck = 100

// Resolver evaluates the path expressions of one Terragrunt file.
type Resolver struct {
	fs    FS
	dir   string
	entry bool
	ctx   *hcl.EvalContext
}

// NewResolver returns a resolver for the file (which may be nil) in dir.
// entry says whether Terragrunt starts from the file (see
// ast.IsEntryFilename): relative paths and the functions that depend on
// the including file resolve only there.
func NewResolver(fsys FS, dir string, file *hcl.File, entry bool) *Resolver {
	r := &Resolver{fs: fsys, dir: dir, entry: entry}
	r.ctx = &hcl.EvalContext{
		Functions: r.functions(),
		Variables: map[string]cty.Value{},
	}
	if file != nil {
		if body, ok := file.Body.(*hclsyntax.Body); ok {
			r.ctx.Variables["local"] = r.staticLocals(body)
		}
	}
	return r
}

// Dir is the directory of the file.
func (r *Resolver) Dir() string {
	return r.dir
}

// IsEntry reports whether the file is an entry file.
func (r *Resolver) IsEntry() bool {
	return r.entry
}

func (r *Resolver) functions() map[string]function.Function {
	fns := map[string]function.Function{
		"dirname":    pathFunc(filepath.Dir),
		"basename":   pathFunc(filepath.Base),
		"format":     stdlib.FormatFunc,
		"join":       stdlib.JoinFunc,
		"lower":      stdlib.LowerFunc,
		"replace":    stdlib.ReplaceFunc,
		"trimprefix": stdlib.TrimPrefixFunc,
		"trimsuffix": stdlib.TrimSuffixFunc,
	}
	if r.entry {
		dirFunc := function.New(&function.Spec{
			Type: function.StaticReturnType(cty.String),
			Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
				return cty.StringVal(r.dir), nil
			},
		})
		fns["get_terragrunt_dir"] = dirFunc
		fns["get_original_terragrunt_dir"] = dirFunc
		fns["get_repo_root"] = function.New(&function.Spec{
			Type: function.StaticReturnType(cty.String),
			Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
				root, ok := r.RepoRoot()
				if !ok {
					return cty.NilVal, errors.New("not in a Git repository")
				}
				return cty.StringVal(root), nil
			},
		})
		fns["find_in_parent_folders"] = function.New(&function.Spec{
			VarParam: &function.Parameter{Name: "args", Type: cty.String},
			Type:     function.StaticReturnType(cty.String),
			Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
				if len(args) == 0 || len(args) > 2 {
					// without a name, Terragrunt looks for a default file
					return cty.NilVal, errors.New("needs a file name")
				}
				if path, ok := r.FindInParentFolders(args[0].AsString()); ok {
					return cty.StringVal(path), nil
				}
				if len(args) == 2 {
					return args[1], nil
				}
				return cty.NilVal, fmt.Errorf("no parent folder has %q", args[0].AsString())
			},
		})
	}
	return fns
}

func pathFunc(f func(string) string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return cty.StringVal(f(args[0].AsString())), nil
		},
	})
}

// staticLocals evaluates the local values of the body which are static:
// known, not null, and made of what the resolver knows. Locals may refer
// to each other in any order, so evaluation repeats until nothing new is
// known.
func (r *Resolver) staticLocals(body *hclsyntax.Body) cty.Value {
	attrs := make(map[string]*hclsyntax.Attribute)
	for _, block := range body.Blocks {
		if block.Type != "locals" {
			continue
		}
		for name, attr := range block.Body.Attributes {
			attrs[name] = attr
		}
	}
	known := make(map[string]cty.Value)
	for progress := true; progress && len(known) < len(attrs); {
		progress = false
		ctx := r.ctx.NewChild()
		ctx.Variables = map[string]cty.Value{"local": cty.ObjectVal(known)}
		for name, attr := range attrs {
			if _, ok := known[name]; ok {
				continue
			}
			v, diags := attr.Expr.Value(ctx)
			if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() {
				continue
			}
			known[name] = v
			progress = true
		}
	}
	return cty.ObjectVal(known)
}

// String returns the value of expr when it is a static string.
func (r *Resolver) String(expr hcl.Expression) (string, bool) {
	if expr == nil {
		return "", false
	}
	v, diags := expr.Value(r.ctx)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}

// Bool returns the value of expr when it is a static bool.
func (r *Resolver) Bool(expr hcl.Expression) (bool, bool) {
	if expr == nil {
		return false, false
	}
	v, diags := expr.Value(r.ctx)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() || v.Type() != cty.Bool {
		return false, false
	}
	return v.True(), true
}

// Path returns the absolute path that expr names, relative to the file's
// directory. A relative path resolves only in entry files: in a file
// which others include, it is relative to the including file.
func (r *Resolver) Path(expr hcl.Expression) (string, bool) {
	s, ok := r.String(expr)
	if !ok || s == "" {
		return "", false
	}
	return r.join(s)
}

func (r *Resolver) join(s string) (string, bool) {
	if filepath.IsAbs(s) {
		return filepath.Clean(s), true
	}
	if !r.entry {
		return "", false
	}
	return filepath.Join(r.dir, s), true
}

// FindInParentFolders looks for name in the parent directories of the
// file's directory, as find_in_parent_folders does: the closest parent
// first, up to Terragrunt's limit, files and directories alike.
func (r *Resolver) FindInParentFolders(name string) (string, bool) {
	prev := r.dir
	for i := 0; i < maxFoldersToCheck; i++ {
		cur := filepath.Dir(prev)
		if cur == prev {
			return "", false
		}
		candidate := filepath.Join(cur, name)
		if _, err := r.fs.Stat(candidate); err == nil {
			return candidate, true
		}
		prev = cur
	}
	return "", false
}

// RepoRoot returns the closest directory from the file's up that holds
// .git (a directory, or a file in a worktree).
func (r *Resolver) RepoRoot() (string, bool) {
	dir := r.dir
	for i := 0; i < maxFoldersToCheck; i++ {
		if _, err := r.fs.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

// LocalSource returns the directory that a local source names, such as
// "../modules//vpc", where "//" separates the directory Terragrunt
// copies from the subdirectory it runs in. Sources with a scheme, a
// forced getter ("git::") or a host are not local.
func (r *Resolver) LocalSource(expr hcl.Expression) (string, bool) {
	s, ok := r.String(expr)
	if !ok {
		return "", false
	}
	if strings.Contains(s, "::") || strings.Contains(s, "://") {
		return "", false
	}
	if !filepath.IsAbs(s) && s != "." && s != ".." &&
		!strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
		// e.g. github.com/org/repo, which go-getter reads from Git
		return "", false
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s[1:], "//"); i >= 0 {
		s = s[:i+1] + "/" + s[i+3:]
	}
	path, ok := r.join(s)
	if !ok {
		return "", false
	}
	fi, err := r.fs.Stat(path)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return path, true
}

// File returns path when it is an existing file.
func (r *Resolver) File(path string) (string, bool) {
	fi, err := r.fs.Stat(path)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return path, true
}

// ConfigIn returns the Terragrunt configuration of a unit or stack
// directory: its terragrunt.hcl, or else its terragrunt.stack.hcl. A
// path to a file is returned as it is.
func (r *Resolver) ConfigIn(path string) (string, bool) {
	fi, err := r.fs.Stat(path)
	if err != nil {
		return "", false
	}
	if !fi.IsDir() {
		return path, true
	}
	for _, name := range []string{"terragrunt.hcl", "terragrunt.stack.hcl"} {
		if f, ok := r.File(filepath.Join(path, name)); ok {
			return f, true
		}
	}
	return "", false
}

// ModuleFileIn returns the file to open for an OpenTofu module directory:
// main.tf or main.tofu, else its first .tf or .tofu file.
func (r *Resolver) ModuleFileIn(dir string) (string, bool) {
	for _, name := range []string{"main.tf", "main.tofu"} {
		if f, ok := r.File(filepath.Join(dir, name)); ok {
			return f, true
		}
	}
	entries, err := r.fs.ReadDir(dir)
	if err != nil {
		return "", false
	}
	names := make([]string, 0)
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".tf") || strings.HasSuffix(e.Name(), ".tofu")) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return filepath.Join(dir, names[0]), true
}

// TextRange is the range of expr to underline: a quoted string without
// its quotes.
func TextRange(expr hclsyntax.Expression, src []byte) hcl.Range {
	return filepaths.TextRange(expr, src)
}
