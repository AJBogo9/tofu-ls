// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Inputs are the values a run of OpenTofu in a root module gets besides
// its configuration and its automatically loaded tfvars files: the files
// given with -var-file, the -var options and the TF_VAR_ environment
// variables.
type Inputs struct {
	// VarFiles are the -var-file paths, relative to the module directory
	// and slash-separated, in the order they are given: a later file
	// overrides an earlier one.
	VarFiles []string
	// Vars are the -var options in the order they are given. A later
	// option or file overrides an earlier one, in the order of the
	// command line.
	Vars []VarFlag
	// EnvVars are the TF_VAR_ environment variables by variable name,
	// with the raw strings OpenTofu parses.
	EnvVars map[string]string
}

// VarFlag is a -var option.
type VarFlag struct {
	// Raw is the option's argument as given, name=value.
	Raw string
	// After is how many of the -var-file files come before the option on
	// the command line.
	After int
}

// InputsSource returns the inputs chosen for a root module directory.
type InputsSource interface {
	Inputs(dir string) Inputs
}

// inputsFS is an FS whose modules get the inputs src chose for them.
type inputsFS struct {
	FS
	src InputsSource
}

// WithInputs returns fsys for LoadModule so that every module it loads,
// callers and called modules included, gets the inputs src chose for its
// directory. A nil src returns fsys.
func WithInputs(fsys FS, src InputsSource) FS {
	if src == nil {
		return fsys
	}
	if s, ok := src.(*InputsStore); ok && s == nil {
		return fsys
	}
	return inputsFS{FS: fsys, src: src}
}

// CleanVarFile returns a -var-file path as the inputs hold it: relative
// to the module directory and slash-separated. Absolute paths and paths
// that leave the module directory are refused, since values are read only
// inside the module's tree.
func CleanVarFile(name string) (string, bool) {
	name = filepath.ToSlash(name)
	if name == "" || path.IsAbs(name) || filepath.IsAbs(name) {
		return "", false
	}
	name = path.Clean(name)
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

// EnvVarsFrom picks the TF_VAR_ variables out of an environment in the
// form os.Environ returns.
func EnvVarsFrom(environ []string) map[string]string {
	vars := make(map[string]string)
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "TF_VAR_") || len(name) == len("TF_VAR_") {
			continue
		}
		vars[strings.TrimPrefix(name, "TF_VAR_")] = val
	}
	return vars
}

// InputsStore holds the -var-file choice and the -var options of each
// root module directory, and the TF_VAR_ variables, when they are read.
// It is safe for concurrent use.
type InputsStore struct {
	mu       sync.RWMutex
	varFiles map[string][]string
	vars     map[string][]VarFlag
	envVars  map[string]string
}

// NewInputsStore returns a store that chooses nothing: only the
// automatically loaded tfvars files count, and no environment variables.
func NewInputsStore() *InputsStore {
	return &InputsStore{varFiles: make(map[string][]string), vars: make(map[string][]VarFlag)}
}

// Set replaces the choice. varFiles maps module directories to their
// -var-file paths (see CleanVarFile; others are dropped), vars to their
// -var options, which count the -var-file paths given before them; envVars
// are the TF_VAR_ variables to use, nil for none. It returns the
// directories whose -var-file files or -var options changed, and whether
// the environment variables changed, which concerns every module.
func (s *InputsStore) Set(varFiles map[string][]string, vars map[string][]VarFlag, envVars map[string]string) ([]string, bool) {
	clean := make(map[string][]string, len(varFiles))
	cleanVars := make(map[string][]VarFlag, len(vars))
	for dir, files := range varFiles {
		var kept []string
		for _, f := range files {
			if name, ok := CleanVarFile(f); ok {
				kept = append(kept, name)
			}
		}
		if len(kept) > 0 {
			clean[filepath.Clean(dir)] = kept
		}
	}
	for dir, flags := range vars {
		if len(flags) == 0 {
			continue
		}
		// count only the files kept among those before each option
		files := varFiles[dir]
		kept := make([]VarFlag, 0, len(flags))
		for _, v := range flags {
			after := 0
			for i := 0; i < v.After && i < len(files); i++ {
				if _, ok := CleanVarFile(files[i]); ok {
					after++
				}
			}
			kept = append(kept, VarFlag{Raw: v.Raw, After: after})
		}
		cleanVars[filepath.Clean(dir)] = kept
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := make(map[string]bool)
	for _, m := range []map[string][]string{clean, s.varFiles} {
		for dir := range m {
			dirs[dir] = true
		}
	}
	for _, m := range []map[string][]VarFlag{cleanVars, s.vars} {
		for dir := range m {
			dirs[dir] = true
		}
	}
	var changed []string
	for dir := range dirs {
		if !equalStrings(s.varFiles[dir], clean[dir]) || !equalFlags(s.vars[dir], cleanVars[dir]) {
			changed = append(changed, dir)
		}
	}
	sort.Strings(changed)
	envChanged := !equalEnv(s.envVars, envVars)
	s.varFiles = clean
	s.vars = cleanVars
	s.envVars = envVars
	return changed, envChanged
}

// Inputs returns the inputs chosen for the module directory dir.
func (s *InputsStore) Inputs(dir string) Inputs {
	if s == nil {
		return Inputs{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Inputs{
		VarFiles: append([]string(nil), s.varFiles[filepath.Clean(dir)]...),
		Vars:     append([]VarFlag(nil), s.vars[filepath.Clean(dir)]...),
		EnvVars:  s.envVars,
	}
}

// Selecting returns the module directories that chose a -var-file inside
// dir, such as the module of envs/prod.tfvars for the directory envs.
func (s *InputsStore) Selecting(dir string) []string {
	if s == nil {
		return nil
	}
	dir = filepath.Clean(dir)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var mods []string
	for mod, files := range s.varFiles {
		for _, f := range files {
			if filepath.Dir(filepath.Join(mod, filepath.FromSlash(f))) == dir {
				mods = append(mods, mod)
				break
			}
		}
	}
	sort.Strings(mods)
	return mods
}

// ModuleOf returns the module directory that chose the file at path as a
// -var-file, and the file's name relative to that module. When several
// modules chose it, the first in lexical order is returned.
func (s *InputsStore) ModuleOf(file string) (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	file = filepath.Clean(file)
	s.mu.RLock()
	defer s.mu.RUnlock()
	dirs := make([]string, 0, len(s.varFiles))
	for dir := range s.varFiles {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		for _, f := range s.varFiles[dir] {
			if filepath.Join(dir, filepath.FromSlash(f)) == file {
				return dir, f, true
			}
		}
	}
	return "", "", false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalFlags(a, b []VarFlag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalEnv(a, b map[string]string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
