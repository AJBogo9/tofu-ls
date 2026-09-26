// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package refactor implements rename of OpenTofu symbols: variables,
// locals, outputs, resources, data sources and module calls.
package refactor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
	tfmod "github.com/opentofu/opentofu-schema/module"
	"github.com/zclconf/go-cty/cty"
)

type SymbolKind string

const (
	KindVariable SymbolKind = "variable"
	KindLocal    SymbolKind = "local"
	KindOutput   SymbolKind = "output"
	KindResource SymbolKind = "resource"
	KindData     SymbolKind = "data"
	KindModule   SymbolKind = "module"
)

// scope is the reference scope of targets declaring the kind,
// as defined by opentofu-schema (refscope).
func (k SymbolKind) scope() lang.ScopeId {
	switch k {
	case KindVariable:
		return "variable"
	case KindLocal:
		return "local"
	case KindOutput:
		return "output"
	case KindResource:
		return "resource"
	case KindData:
		return "data"
	case KindModule:
		return "module"
	}
	return ""
}

// nameIndex is the index of the name step in the symbol's address,
// e.g. 1 in var.name and aws_instance.name, 2 in data.aws_ami.name.
func (k SymbolKind) nameIndex() int {
	if k == KindData {
		return 2
	}
	return 1
}

// labelIndex is the index of the block label holding the name.
func (k SymbolKind) labelIndex() int {
	if k == KindResource || k == KindData {
		return 1
	}
	return 0
}

// movable tells whether renaming changes the address of real
// infrastructure, so that a moved block must keep it.
func (k SymbolKind) movable() bool {
	return k == KindResource || k == KindModule
}

// Env gives rename access to the indexed configuration.
type Env struct {
	Decoder    *decoder.Decoder
	PathReader decoder.PathReader
	// ReadFile reads a file by absolute path, preferring the open document.
	ReadFile func(path string) ([]byte, error)
	// ModuleCalls returns the module calls declared in a module directory.
	ModuleCalls func(modPath string) (map[string]tfmod.DeclaredModuleCall, error)
	// ReadDir lists a directory, for var files that are not indexed. It
	// may be nil.
	ReadDir func(dir string) ([]fs.DirEntry, error)
	// InWorkspace reports whether a directory is inside one of the
	// workspace folders. It may be nil, and then every directory is.
	InWorkspace func(dir string) bool
	// RelPath names a file for messages, relative to the workspace folder
	// which holds it, or returns "" when no folder does. It may be nil.
	// Without a name from it, messages name files relative to the module
	// the request is about.
	RelPath func(path string) string
}

// Symbol is a renamable declaration.
type Symbol struct {
	Kind SymbolKind
	// Path is the module (with the opentofu language ID) which declares it.
	Path lang.Path
	// Addr is the address of the declaration as the module refers to it:
	// var.x, local.x, output.x, aws_instance.x, data.aws_ami.x or module.x.
	Addr lang.Address
	Name string
	// NameRange is the name in the declaration: the label without quotes,
	// or the attribute name of a local value.
	NameRange hcl.Range
	// CursorRange is the name at the position rename was invoked on,
	// in the file of the request.
	CursorRange hcl.Range

	target reference.Target
}

// Edit replaces Range (in the file at the absolute path File) with NewText.
type Edit struct {
	File    string
	Range   hcl.Range
	NewText string
}

type Options struct {
	// AddMovedBlock appends a moved block after renaming
	// a resource or a module call.
	AddMovedBlock bool
}

// ErrNotRenamable is returned when the position is not on a name
// which can be renamed.
var ErrNotRenamable = errors.New("this element cannot be renamed")

// fileCache reads and parses each file once per request.
type fileCache struct {
	env   Env
	src   map[string][]byte
	files map[string]*hcl.File
	// base is the module directory that messages name files relative to.
	base string
}

func newFileCache(env Env) *fileCache {
	return &fileCache{env: env, src: map[string][]byte{}, files: map[string]*hcl.File{}}
}

func (c *fileCache) source(path string) ([]byte, error) {
	if src, ok := c.src[path]; ok {
		return src, nil
	}
	src, err := c.env.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c.src[path] = src
	return src, nil
}

// displayPath names a file for messages, relative to the workspace (or
// else to the module the request is about), so that main.tf of a child
// module is not mistaken for the open main.tf, nor the other way round.
func (c *fileCache) displayPath(path string) string {
	if c.env.RelPath != nil {
		if rel := c.env.RelPath(path); rel != "" {
			return rel
		}
	}
	if c.base != "" {
		if rel, err := filepath.Rel(c.base, path); err == nil {
			return rel
		}
	}
	return path
}

func (c *fileCache) body(path string) (*hclsyntax.Body, []byte, error) {
	src, err := c.source(path)
	if err != nil {
		return nil, nil, err
	}
	if strings.HasSuffix(path, ".json") {
		return nil, nil, fmt.Errorf("rename in JSON configuration (%s) is not supported", filepath.Base(path))
	}
	f, ok := c.files[path]
	if !ok {
		var diags hcl.Diagnostics
		f, diags = hclsyntax.ParseConfig(src, filepath.Base(path), hcl.InitialPos)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("%s has syntax errors; fix them before renaming", c.displayPath(path))
		}
		c.files[path] = f
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected body in %s", filepath.Base(path))
	}
	return body, src, nil
}

// FindSymbol returns the renamable symbol at pos, or ErrNotRenamable. It
// refuses symbols declared in a module the user does not edit: one
// installed under .terraform (from a registry or git), or one outside the
// workspace, whose other callers cannot be seen.
func FindSymbol(ctx context.Context, env Env, path lang.Path, file string, pos hcl.Pos) (*Symbol, error) {
	fc := newFileCache(env)
	fc.base = path.Path
	sym, err := findSymbol(ctx, env, fc, path, file, pos)
	if err != nil {
		return nil, err
	}
	if isInstalledModule(sym.Path.Path) {
		return nil, fmt.Errorf("%s is declared in a module installed by tofu init (%s); rename it in the module's source instead", sym.Addr.String(), sym.Path.Path)
	}
	if env.InWorkspace != nil && !env.InWorkspace(sym.Path.Path) {
		return nil, fmt.Errorf("%s is declared in %s, outside the workspace, where other callers of the module cannot be seen; open that folder to rename it", sym.Addr.String(), sym.Path.Path)
	}
	return sym, nil
}

// isInstalledModule reports whether a module directory was installed by
// tofu init under .terraform, for example from a registry or git.
func isInstalledModule(dir string) bool {
	sep := string(filepath.Separator)
	return strings.Contains(filepath.Clean(dir)+sep, sep+".terraform"+sep)
}

func findSymbol(ctx context.Context, env Env, fc *fileCache, path lang.Path, file string, pos hcl.Pos) (*Symbol, error) {
	pathCtx, err := env.PathReader.PathContext(path)
	if err != nil {
		return nil, err
	}

	origins, onOrigin := pathCtx.ReferenceOrigins.AtPos(file, pos)
	if !onOrigin && !strings.HasSuffix(file, ".json") && !strings.HasSuffix(file, ".tfvars") {
		// a reference the schema does not decode, e.g. in a validation
		// condition: read it from the syntax instead
		if origin, ok := syntaxOriginAtPos(fc, filepath.Join(path.Path, file), pos); ok {
			origins, onOrigin = reference.Origins{origin}, true
		}
	}
	if onOrigin {
		for _, origin := range origins {
			sym, err := symbolFromOrigin(env, fc, path, pathCtx, origin, pos)
			if err != nil {
				return nil, err
			}
			if sym != nil {
				return sym, nil
			}
		}
		return nil, ErrNotRenamable
	}

	targets := pathCtx.ReferenceTargets
	if target, ok := checkScopedDataTarget(fc, path.Path, pathCtx, nil, &hcl.Range{Filename: file, Start: pos, End: pos}); ok {
		targets = append(targets[:len(targets):len(targets)], target)
	}
	for _, target := range targets {
		kind, ok := kindOfTarget(target)
		if !ok || target.RangePtr == nil || target.RangePtr.Filename != file {
			continue
		}
		if !target.RangePtr.ContainsPos(pos) {
			continue
		}
		sym, err := symbolFromTarget(fc, path, kind, target)
		if err != nil {
			return nil, err
		}
		if sym.NameRange.ContainsPos(pos) || sym.NameRange.End == pos {
			sym.CursorRange = sym.NameRange
			return sym, nil
		}
	}

	return nil, ErrNotRenamable
}

// syntaxOriginAtPos returns the reference at pos as read from the
// file's syntax, for references the schema does not decode.
func syntaxOriginAtPos(fc *fileCache, file string, pos hcl.Pos) (reference.LocalOrigin, bool) {
	body, _, err := fc.body(file)
	if err != nil {
		return reference.LocalOrigin{}, false
	}
	var found reference.LocalOrigin
	ok := false
	_ = walkBodyTraversals(body, "", false, func(tr hcl.Traversal, _ bool) error {
		rng := tr.SourceRange()
		if ok || !(rng.ContainsPos(pos) || rng.End == pos) {
			return nil
		}
		addr, err := lang.TraversalToAddress(tr)
		if err != nil {
			return nil
		}
		found = reference.LocalOrigin{Addr: addr, Range: rng}
		ok = true
		return nil
	})
	return found, ok
}

// kindOfTarget recognizes top-level declarations.
func kindOfTarget(target reference.Target) (SymbolKind, bool) {
	addr := target.Addr
	switch {
	case len(addr) == 2 && addr[0].String() == "var" && target.ScopeId == KindVariable.scope():
		return KindVariable, true
	case len(addr) == 2 && addr[0].String() == "local" && target.ScopeId == KindLocal.scope():
		return KindLocal, true
	case len(addr) == 2 && addr[0].String() == "output" && target.ScopeId == KindOutput.scope():
		return KindOutput, true
	case len(addr) == 2 && addr[0].String() == "module" && target.ScopeId == KindModule.scope():
		return KindModule, true
	case len(addr) == 3 && addr[0].String() == "data" && target.ScopeId == KindData.scope():
		return KindData, true
	case len(addr) == 2 && target.ScopeId == KindResource.scope():
		return KindResource, true
	}
	return "", false
}

// checkScopedDataTarget finds a data source declared inside a check block,
// which the schema declares no reference target for: by its address, or
// (with addr nil) by a label range holding the position at.
func checkScopedDataTarget(fc *fileCache, modPath string, pathCtx *decoder.PathContext, addr lang.Address, at *hcl.Range) (reference.Target, bool) {
	for _, name := range sortedHCLFiles(pathCtx) {
		if at != nil && name != at.Filename {
			continue
		}
		body, _, err := fc.body(filepath.Join(modPath, name))
		if err != nil {
			continue
		}
		for _, check := range body.Blocks {
			if check.Type != "check" {
				continue
			}
			for _, block := range check.Body.Blocks {
				if block.Type != "data" || len(block.Labels) != 2 {
					continue
				}
				dataAddr := lang.Address{
					lang.RootStep{Name: "data"},
					lang.AttrStep{Name: block.Labels[0]},
					lang.AttrStep{Name: block.Labels[1]},
				}
				if addr != nil && !dataAddr.Equals(addr) {
					continue
				}
				if at != nil && !block.LabelRanges[1].ContainsPos(at.Start) {
					continue
				}
				rng := block.Range()
				rng.Filename = name
				return reference.Target{Addr: dataAddr, ScopeId: KindData.scope(), RangePtr: &rng}, true
			}
		}
	}
	return reference.Target{}, false
}

// declaredTarget finds the declaration of addr with the kind's scope.
func declaredTarget(pathCtx *decoder.PathContext, kind SymbolKind, addr lang.Address) (reference.Target, bool) {
	for _, target := range pathCtx.ReferenceTargets {
		if target.RangePtr == nil || !target.Addr.Equals(addr) {
			continue
		}
		if k, ok := kindOfTarget(target); ok && k == kind {
			return target, true
		}
	}
	return reference.Target{}, false
}

func symbolFromOrigin(env Env, fc *fileCache, path lang.Path, pathCtx *decoder.PathContext, origin reference.Origin, pos hcl.Pos) (*Symbol, error) {
	switch o := origin.(type) {
	case reference.PathOrigin:
		// a tfvars key or a module input argument, naming a variable
		if len(o.TargetAddr) != 2 || o.TargetAddr[0].String() != "var" {
			return nil, nil
		}
		targetCtx, err := env.PathReader.PathContext(o.TargetPath)
		if err != nil {
			return nil, nil
		}
		target, ok := declaredTarget(targetCtx, KindVariable, o.TargetAddr)
		if !ok {
			return nil, nil
		}
		sym, err := symbolFromTarget(fc, o.TargetPath, KindVariable, target)
		if err != nil {
			return nil, err
		}
		src, err := fc.source(filepath.Join(path.Path, o.Range.Filename))
		if err != nil {
			return nil, err
		}
		rng, ok := nameRangeInKey(src, o.Range, sym.Name)
		if !ok {
			return nil, nil
		}
		sym.CursorRange = rng
		return sym, nil
	case reference.LocalOrigin:
		src, err := fc.source(filepath.Join(path.Path, o.Range.Filename))
		if err != nil {
			return nil, err
		}
		steps, ok := traversalSteps(src, o.Range)
		if !ok {
			return nil, nil
		}
		stepIdx := -1
		for i, rng := range steps {
			if rng.ContainsPos(pos) || rng.End == pos {
				stepIdx = i
				break
			}
		}
		if stepIdx < 0 {
			return nil, nil
		}
		addr := o.Addr
		if len(addr) == 0 {
			return nil, nil
		}
		var kind SymbolKind
		switch addr[0].String() {
		case "var":
			kind = KindVariable
		case "local":
			if onlyProviderConstraints(o.Constraints) {
				return nil, nil
			}
			kind = KindLocal
		case "module":
			kind = KindModule
			if stepIdx >= 2 && len(addr) >= 3 {
				return outputSymbolFromOrigin(env, fc, path, addr, steps[2])
			}
		case "data":
			kind = KindData
		case "each", "count", "self", "path", "terraform":
			return nil, nil
		default:
			kind = KindResource
		}
		nameIdx := kind.nameIndex()
		if stepIdx > nameIdx || len(addr) <= nameIdx || len(steps) <= nameIdx {
			// e.g. an attribute of a resource, which is not a declaration
			return nil, nil
		}
		symAddr := addr.FirstSteps(uint(nameIdx + 1))
		target, ok := declaredTarget(pathCtx, kind, symAddr)
		if !ok && kind == KindData {
			target, ok = checkScopedDataTarget(fc, path.Path, pathCtx, symAddr, nil)
		}
		if !ok {
			return nil, nil
		}
		sym, err := symbolFromTarget(fc, path, kind, target)
		if err != nil {
			return nil, err
		}
		sym.CursorRange = steps[nameIdx]
		return sym, nil
	}
	return nil, nil
}

func outputSymbolFromOrigin(env Env, fc *fileCache, path lang.Path, addr lang.Address, cursorRng hcl.Range) (*Symbol, error) {
	childPath, ok := localModuleCallPath(env, path.Path, stepName(addr[1]))
	if !ok {
		return nil, nil
	}
	childLangPath := lang.Path{Path: childPath, LanguageID: path.LanguageID}
	childCtx, err := env.PathReader.PathContext(childLangPath)
	if err != nil {
		return nil, nil
	}
	outAddr := lang.Address{lang.RootStep{Name: "output"}, lang.AttrStep{Name: stepName(addr[2])}}
	target, ok := declaredTarget(childCtx, KindOutput, outAddr)
	if !ok {
		return nil, nil
	}
	sym, err := symbolFromTarget(fc, childLangPath, KindOutput, target)
	if err != nil {
		return nil, err
	}
	sym.CursorRange = cursorRng
	return sym, nil
}

// localModuleCallPath returns the directory of a module call with a local source.
func localModuleCallPath(env Env, modPath, callName string) (string, bool) {
	if env.ModuleCalls == nil {
		return "", false
	}
	calls, err := env.ModuleCalls(modPath)
	if err != nil {
		return "", false
	}
	call, ok := calls[callName]
	if !ok {
		return "", false
	}
	src, ok := call.SourceAddr.(tfmod.LocalSourceAddr)
	if !ok {
		return "", false
	}
	return filepath.Clean(filepath.Join(modPath, src.String())), true
}

func onlyProviderConstraints(cons reference.OriginConstraints) bool {
	if len(cons) == 0 {
		return false
	}
	for _, c := range cons {
		if c.OfScopeId != "provider" {
			return false
		}
	}
	return true
}

// symbolFromTarget locates the name of a declaration in its file.
func symbolFromTarget(fc *fileCache, path lang.Path, kind SymbolKind, target reference.Target) (*Symbol, error) {
	file := filepath.Join(path.Path, target.RangePtr.Filename)
	body, src, err := fc.body(file)
	if err != nil {
		return nil, err
	}
	name := stepName(target.Addr[kind.nameIndex()])
	sym := &Symbol{
		Kind:   kind,
		Path:   lang.Path{Path: path.Path, LanguageID: path.LanguageID},
		Addr:   target.Addr,
		Name:   name,
		target: target,
	}

	start := target.RangePtr.Start
	blocks := body.Blocks
	if kind == KindData {
		// data sources scoped to a check block
		for _, block := range body.Blocks {
			if block.Type == "check" {
				blocks = append(blocks[:len(blocks):len(blocks)], block.Body.Blocks...)
			}
		}
	}
	for _, block := range blocks {
		if kind == KindLocal {
			if block.Type != "locals" {
				continue
			}
			for _, attr := range block.Body.Attributes {
				if attr.Name == name && attr.SrcRange.Start.Byte == start.Byte {
					sym.NameRange = attr.NameRange
					return sym, nil
				}
			}
			continue
		}
		if block.Range().Start.Byte != start.Byte {
			continue
		}
		i := kind.labelIndex()
		if len(block.LabelRanges) <= i {
			break
		}
		rng, ok := unquotedLabelRange(src, block.LabelRanges[i], name)
		if !ok {
			break
		}
		sym.NameRange = rng
		return sym, nil
	}

	return nil, fmt.Errorf("declaration of %s not found in %s", target.Addr.String(), target.RangePtr.Filename)
}

// unquotedLabelRange returns the range of the label's name, inside its quotes.
func unquotedLabelRange(src []byte, rng hcl.Range, name string) (hcl.Range, bool) {
	text := string(src[rng.Start.Byte:rng.End.Byte])
	if text == name {
		// unquoted label
		return rng, true
	}
	if text != `"`+name+`"` {
		return hcl.Range{}, false
	}
	inner := rng
	inner.Start = shiftPos(rng.Start, 1)
	inner.End = shiftPos(rng.End, -1)
	return inner, true
}

// nameRangeInKey returns the range of name in a key, e.g. a tfvars key
// or a module input argument, which may be quoted.
func nameRangeInKey(src []byte, rng hcl.Range, name string) (hcl.Range, bool) {
	return unquotedLabelRange(src, rng, name)
}

func shiftPos(pos hcl.Pos, n int) hcl.Pos {
	return hcl.Pos{Line: pos.Line, Column: pos.Column + n, Byte: pos.Byte + n}
}

// traversalSteps parses the reference in rng and returns the range of
// each step's name (without the leading dot of an attribute step).
func traversalSteps(src []byte, rng hcl.Range) ([]hcl.Range, bool) {
	if rng.End.Byte > len(src) || rng.Start.Byte >= rng.End.Byte {
		return nil, false
	}
	traversal, diags := hclsyntax.ParseTraversalPartial(src[rng.Start.Byte:rng.End.Byte], rng.Filename, rng.Start)
	if diags.HasErrors() && len(traversal) == 0 {
		return nil, false
	}
	steps := make([]hcl.Range, 0, len(traversal))
	for _, step := range traversal {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			steps = append(steps, s.SrcRange)
		case hcl.TraverseAttr:
			r := s.SrcRange
			if r.End.Byte-r.Start.Byte == len(s.Name)+1 {
				r.Start = shiftPos(r.Start, 1)
			}
			steps = append(steps, r)
		default:
			// index and splat steps end the named part
			return steps, len(steps) > 0
		}
	}
	return steps, len(steps) > 0
}

// reservedVariableNames cannot be used as variable names,
// because module blocks use them as meta-arguments.
var reservedVariableNames = map[string]bool{
	"source": true, "version": true, "providers": true, "count": true,
	"for_each": true, "lifecycle": true, "depends_on": true, "locals": true,
}

// ValidateName checks that name is a valid new name for the symbol kind.
func ValidateName(kind SymbolKind, name string) error {
	if !hclsyntax.ValidIdentifier(name) {
		return fmt.Errorf("%q is not a valid name: use letters, digits, underscores and dashes, starting with a letter or underscore", name)
	}
	if kind == KindVariable && reservedVariableNames[name] {
		return fmt.Errorf("%q is reserved and cannot be a variable name", name)
	}
	return nil
}

// Rename computes the edits which rename sym to newName across all
// indexed modules.
func Rename(ctx context.Context, env Env, sym *Symbol, newName string, opts Options) ([]Edit, error) {
	if newName == sym.Name {
		return []Edit{}, nil
	}
	if err := ValidateName(sym.Kind, newName); err != nil {
		return nil, err
	}

	pathCtx, err := env.PathReader.PathContext(sym.Path)
	if err != nil {
		return nil, err
	}
	nameIdx := sym.Kind.nameIndex()
	newAddr := make(lang.Address, len(sym.Addr))
	copy(newAddr, sym.Addr)
	if nameIdx == 0 {
		newAddr[nameIdx] = lang.RootStep{Name: newName}
	} else {
		newAddr[nameIdx] = lang.AttrStep{Name: newName}
	}
	fc := newFileCache(env)
	fc.base = sym.Path.Path
	_, exists := declaredTarget(pathCtx, sym.Kind, newAddr)
	if !exists && sym.Kind == KindData {
		_, exists = checkScopedDataTarget(fc, sym.Path.Path, pathCtx, newAddr, nil)
	}
	if exists {
		return nil, fmt.Errorf("%s already exists", newAddr.String())
	}
	if sym.Kind.movable() {
		if err := historyCollision(fc, sym.Path.Path, pathCtx, sym.Addr, newAddr); err != nil {
			return nil, err
		}
	}
	edits := newEditSet()
	declFile := filepath.Join(sym.Path.Path, sym.target.RangePtr.Filename)
	edits.add(declFile, sym.NameRange, newName)

	addMoved := opts.AddMovedBlock && sym.Kind.movable()

	// references within the declaring module: every traversal in every
	// file, so that places the schema does not decode (validation
	// conditions, schema-less blocks) are renamed too
	if sym.Kind != KindOutput {
		prefix := addrNames(sym.Addr)
		err := walkModuleTraversals(fc, sym.Path.Path, pathCtx, func(file string, tr hcl.Traversal, inMoved bool) error {
			if addMoved && inMoved {
				// keeps the chain: from = older, to = old, then old -> new
				return nil
			}
			if rng, ok := stepRangeIfPrefix(tr, prefix, nameIdx); ok {
				edits.add(file, rng, newName)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// the same declaration in override files, which OpenTofu merges into
	// this one and which must keep its address
	if err := renameOverrides(fc, sym, declFile, pathCtx, newName, edits); err != nil {
		return nil, err
	}

	// test files: run blocks refer to the module's objects, and variables
	// blocks set its variables
	for _, file := range moduleTestFiles(env, sym.Path.Path) {
		body, _, err := fc.body(file)
		if err != nil {
			return nil, err
		}
		prefix := addrNames(sym.Addr)
		_ = walkBodyTraversals(body, "", false, func(tr hcl.Traversal, _ bool) error {
			if rng, ok := stepRangeIfPrefix(tr, prefix, nameIdx); ok {
				edits.add(file, rng, newName)
			}
			return nil
		})
		if sym.Kind == KindVariable {
			for _, rng := range testVariableKeys(body, sym.Name) {
				edits.add(file, rng, newName)
			}
		}
	}

	// tfvars keys and module input arguments of parent modules
	if sym.Kind == KindVariable {
		pathOrigins := make([]decoder.PathOrigin, 0)
		for _, target := range pathCtx.ReferenceTargets {
			// a variable is declared as several targets (type-less and typed)
			if k, ok := kindOfTarget(target); ok && k == sym.Kind && target.Addr.Equals(sym.Addr) {
				pathOrigins = append(pathOrigins, env.Decoder.OriginsTargeting(ctx, target, sym.Path)...)
			}
		}
		for _, po := range pathOrigins {
			origin, ok := po.Origin.(reference.PathOrigin)
			if !ok {
				continue
			}
			file := filepath.Join(po.Path.Path, origin.Range.Filename)
			src, err := fc.source(file)
			if err != nil {
				return nil, err
			}
			rng, ok := nameRangeInKey(src, origin.Range, sym.Name)
			if !ok {
				continue
			}
			edits.add(file, rng, newName)
		}
		// var files in subdirectories (such as envs/prod.tfvars) are not
		// indexed, but -var-file uses them with this module
		for _, file := range subdirVarFiles(env, sym.Path.Path) {
			src, err := fc.source(file)
			if err != nil {
				continue
			}
			for _, rng := range varFileKeyRanges(file, src, sym.Name) {
				edits.add(file, rng, newName)
			}
		}
	}

	// module.<call>.<output> in modules calling this one
	if sym.Kind == KindOutput {
		for _, p := range env.PathReader.Paths(ctx) {
			if p.LanguageID != sym.Path.LanguageID || p.Path == sym.Path.Path {
				continue
			}
			calls := callsOfModule(env, p.Path, sym.Path.Path)
			if len(calls) == 0 {
				continue
			}
			parentCtx, err := env.PathReader.PathContext(p)
			if err != nil {
				continue
			}
			for call := range calls {
				if file, rng, ok := forExprOverModule(fc, p.Path, parentCtx, call); ok {
					return nil, fmt.Errorf("module.%s is iterated by a for expression (%s:%d), whose uses of %s cannot be renamed safely; rename them by hand", call, fc.displayPath(file), rng.Start.Line, sym.Name)
				}
				err := walkModuleTraversals(fc, p.Path, parentCtx, func(file string, tr hcl.Traversal, _ bool) error {
					if rng, ok := moduleOutputStepRange(tr, call, sym.Name); ok {
						edits.add(file, rng, newName)
					}
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
		}
	}

	if addMoved {
		src, err := fc.source(declFile)
		if err != nil {
			return nil, err
		}
		edits.add(declFile, endOfFile(src, sym.target.RangePtr.Filename), movedBlock(src, sym.Addr, newAddr))
	}

	return edits.list(), nil
}

// moduleOutputStepRange returns the range of the output name (without the
// dot) in module.<call>.<output>, module.<call>[key].<output> or
// module.<call>[*].<output>.
func moduleOutputStepRange(tr hcl.Traversal, call, output string) (hcl.Range, bool) {
	if len(tr) < 3 {
		return hcl.Range{}, false
	}
	if root, ok := tr[0].(hcl.TraverseRoot); !ok || root.Name != "module" {
		return hcl.Range{}, false
	}
	if c, ok := tr[1].(hcl.TraverseAttr); !ok || c.Name != call {
		return hcl.Range{}, false
	}
	i := 2
	switch tr[i].(type) {
	case hcl.TraverseIndex, hcl.TraverseSplat:
		i++
	}
	if i >= len(tr) {
		return hcl.Range{}, false
	}
	out, ok := tr[i].(hcl.TraverseAttr)
	if !ok || out.Name != output {
		return hcl.Range{}, false
	}
	rng := out.SrcRange
	if rng.End.Byte-rng.Start.Byte == len(out.Name)+1 {
		rng.Start = shiftPos(rng.Start, 1)
	}
	return rng, true
}

// forExprOverModule finds a for expression in the module at modPath whose
// collection is module.<call> itself, so that the output is reached
// through the iteration symbol, which rename cannot follow.
func forExprOverModule(fc *fileCache, modPath string, pathCtx *decoder.PathContext, call string) (string, hcl.Range, bool) {
	for _, name := range sortedHCLFiles(pathCtx) {
		file := filepath.Join(modPath, name)
		body, _, err := fc.body(file)
		if err != nil {
			continue
		}
		var found *hcl.Range
		hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
			fe, ok := node.(*hclsyntax.ForExpr)
			if !ok || found != nil {
				return nil
			}
			if coll, ok := fe.CollExpr.(*hclsyntax.ScopeTraversalExpr); ok && len(coll.Traversal) == 2 &&
				coll.Traversal.RootName() == "module" {
				if c, ok := coll.Traversal[1].(hcl.TraverseAttr); ok && c.Name == call {
					rng := fe.SrcRange
					found = &rng
				}
			}
			return nil
		})
		if found != nil {
			return file, *found, true
		}
	}
	return "", hcl.Range{}, false
}

// isOverrideFile reports whether a configuration file is an override
// file (override.tf, *_override.tf and their .tofu forms).
func isOverrideFile(name string) bool {
	for _, ext := range []string{".tf", ".tofu"} {
		if strings.HasSuffix(name, ext) {
			base := strings.TrimSuffix(filepath.Base(name), ext)
			return base == "override" || strings.HasSuffix(base, "_override")
		}
	}
	return false
}

// renameOverrides renames the labels (or, for a local, the attribute
// name) of the declarations with the symbol's address in the module's
// override files, which OpenTofu merges into the primary declaration.
func renameOverrides(fc *fileCache, sym *Symbol, declFile string, pathCtx *decoder.PathContext, newName string, edits *editSet) error {
	for _, name := range sortedHCLFiles(pathCtx) {
		file := filepath.Join(sym.Path.Path, name)
		if !isOverrideFile(name) || file == declFile {
			continue
		}
		body, src, err := fc.body(file)
		if err != nil {
			return err
		}
		for _, block := range body.Blocks {
			if sym.Kind == KindLocal {
				if block.Type != "locals" {
					continue
				}
				if attr, ok := block.Body.Attributes[sym.Name]; ok {
					edits.add(file, attr.NameRange, newName)
				}
				continue
			}
			if block.Type != string(sym.Kind) {
				continue
			}
			i := sym.Kind.labelIndex()
			if len(block.Labels) != i+1 || block.Labels[i] != sym.Name {
				continue
			}
			if sym.Kind == KindResource || sym.Kind == KindData {
				if block.Labels[0] != stepName(sym.Addr[sym.Kind.nameIndex()-1]) {
					continue
				}
			}
			if rng, ok := unquotedLabelRange(src, block.LabelRanges[i], sym.Name); ok {
				edits.add(file, rng, newName)
			}
		}
	}
	return nil
}

// moduleTestFiles returns the test files (*.tftest.hcl, *.tofutest.hcl)
// of a module: next to it and in its tests directory.
func moduleTestFiles(env Env, modPath string) []string {
	if env.ReadDir == nil {
		return nil
	}
	var files []string
	for _, dir := range []string{modPath, filepath.Join(modPath, "tests")} {
		entries, err := env.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() && (strings.HasSuffix(name, ".tftest.hcl") || strings.HasSuffix(name, ".tofutest.hcl")) {
				files = append(files, filepath.Join(dir, name))
			}
		}
	}
	sort.Strings(files)
	return files
}

// testVariableKeys returns the ranges of the keys called name in the
// variables blocks of a test file, at the top level and in run blocks.
func testVariableKeys(body *hclsyntax.Body, name string) []hcl.Range {
	var ranges []hcl.Range
	for _, block := range body.Blocks {
		switch block.Type {
		case "variables":
			if attr, ok := block.Body.Attributes[name]; ok {
				ranges = append(ranges, attr.NameRange)
			}
		case "run":
			ranges = append(ranges, testVariableKeys(block.Body, name)...)
		}
	}
	return ranges
}

// maxVarFileDepth is how many directory levels below a module rename
// looks for var files.
const maxVarFileDepth = 3

// subdirVarFiles returns the var files (*.tfvars, *.tfvars.json) in the
// subdirectories of a module, skipping hidden directories and
// subdirectories that hold their own configuration (other modules). The
// language server indexes only the var files next to the module.
func subdirVarFiles(env Env, modPath string) []string {
	if env.ReadDir == nil {
		return nil
	}
	var files []string
	var walk func(dir string, depth int, top bool)
	walk = func(dir string, depth int, top bool) {
		entries, err := env.ReadDir(dir)
		if err != nil {
			return
		}
		var found []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || top {
				continue
			}
			switch {
			case strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tofu"):
				// another module: its var files are its own
				return
			case strings.HasSuffix(name, ".tfvars") || strings.HasSuffix(name, ".tfvars.json"):
				found = append(found, filepath.Join(dir, name))
			}
		}
		files = append(files, found...)
		if depth >= maxVarFileDepth {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			walk(filepath.Join(dir, name), depth+1, false)
		}
	}
	walk(modPath, 0, true)
	sort.Strings(files)
	return files
}

// varFileKeyRanges returns the range of every top-level key called name
// in a var file, without quotes.
func varFileKeyRanges(file string, src []byte, name string) []hcl.Range {
	var f *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(file, ".json") {
		f, diags = hcljson.Parse(src, file)
	} else {
		f, diags = hclsyntax.ParseConfig(src, file, hcl.InitialPos)
	}
	if diags.HasErrors() || f == nil {
		return nil
	}
	attrs, _ := f.Body.JustAttributes()
	attr, ok := attrs[name]
	if !ok {
		return nil
	}
	rng, ok := unquotedLabelRange(src, attr.NameRange, name)
	if !ok {
		return nil
	}
	return []hcl.Range{rng}
}

// historyCollision refuses a new address that a moved or removed block of
// the module already names as its from address: OpenTofu would treat the
// renamed object as moved away or removed, and a moved block back to the
// old name would make a cycle. Whether such a block has been applied is in
// the state, which rename cannot see, so the user decides.
func historyCollision(fc *fileCache, modPath string, pathCtx *decoder.PathContext, oldAddr, newAddr lang.Address) error {
	for _, name := range sortedHCLFiles(pathCtx) {
		file := filepath.Join(modPath, name)
		body, _, err := fc.body(file)
		if err != nil {
			return err
		}
		for _, block := range body.Blocks {
			if block.Type != "moved" && block.Type != "removed" {
				continue
			}
			from, ok := block.Body.Attributes["from"]
			if !ok {
				continue
			}
			tr, diags := hcl.AbsTraversalForExpr(from.Expr)
			if diags.HasErrors() || traversalWithoutKeys(tr) != newAddr.String() {
				continue
			}
			where := fmt.Sprintf("%s:%d", fc.displayPath(file), block.DefRange().Start.Line)
			if block.Type == "removed" {
				return fmt.Errorf("%s is the from address of a removed block (%s), so OpenTofu would remove the renamed object; pick another name", newAddr.String(), where)
			}
			if to, ok := block.Body.Attributes["to"]; ok {
				if toTr, diags := hcl.AbsTraversalForExpr(to.Expr); !diags.HasErrors() && traversalWithoutKeys(toTr) == oldAddr.String() {
					return fmt.Errorf("the moved block at %s moves %s to %s, so renaming back would make a cycle; delete that moved block first if it has not been applied, or pick another name", where, newAddr.String(), oldAddr.String())
				}
			}
			return fmt.Errorf("%s is the from address of a moved block (%s), so OpenTofu would move the renamed object; pick another name", newAddr.String(), where)
		}
	}
	return nil
}

// traversalWithoutKeys renders the names of a traversal, leaving out
// instance keys: module.app[0].terraform_data.web is
// module.app.terraform_data.web.
func traversalWithoutKeys(tr hcl.Traversal) string {
	names := make([]string, 0, len(tr))
	for _, step := range tr {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			names = append(names, s.Name)
		case hcl.TraverseAttr:
			names = append(names, s.Name)
		}
	}
	return strings.Join(names, ".")
}

// addrNames returns the step names of a root/attribute address.
func addrNames(addr lang.Address) []string {
	names := make([]string, len(addr))
	for i, step := range addr {
		names[i] = stepName(step)
	}
	return names
}

// stepRangeIfPrefix returns the range of step i of the traversal
// (without the dot) when the traversal starts with the names in prefix.
func stepRangeIfPrefix(tr hcl.Traversal, prefix []string, i int) (hcl.Range, bool) {
	if len(tr) < len(prefix) {
		return hcl.Range{}, false
	}
	var rng hcl.Range
	for j, name := range prefix {
		switch s := tr[j].(type) {
		case hcl.TraverseRoot:
			if s.Name != name {
				return hcl.Range{}, false
			}
			rng = s.SrcRange
		case hcl.TraverseAttr:
			if s.Name != name {
				return hcl.Range{}, false
			}
			rng = s.SrcRange
			if rng.End.Byte-rng.Start.Byte == len(s.Name)+1 {
				rng.Start = shiftPos(rng.Start, 1)
			}
		default:
			return hcl.Range{}, false
		}
		if j == i {
			return rng, true
		}
	}
	return hcl.Range{}, false
}

// walkModuleTraversals calls fn for every absolute traversal in the
// module's HCL files, except provider references in the provider and
// providers meta-arguments (provider "local" would otherwise look like
// local.* values).
func walkModuleTraversals(fc *fileCache, modPath string, pathCtx *decoder.PathContext, fn func(file string, tr hcl.Traversal, inMoved bool) error) error {
	for _, name := range sortedHCLFiles(pathCtx) {
		file := filepath.Join(modPath, name)
		body, _, err := fc.body(file)
		if err != nil {
			return err
		}
		if err := walkBodyTraversals(body, "", false, func(tr hcl.Traversal, inMoved bool) error {
			return fn(file, tr, inMoved)
		}); err != nil {
			return err
		}
	}
	return nil
}

// sortedHCLFiles returns the module's native syntax files in lexical order.
func sortedHCLFiles(pathCtx *decoder.PathContext) []string {
	filenames := make([]string, 0, len(pathCtx.Files))
	for name := range pathCtx.Files {
		if strings.HasSuffix(name, ".json") {
			continue
		}
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)
	return filenames
}

func walkBodyTraversals(body *hclsyntax.Body, blockType string, inMoved bool, fn func(tr hcl.Traversal, inMoved bool) error) error {
	for name, attr := range body.Attributes {
		var providerAddrs map[hcl.Range]bool
		if isProviderMetaArgument(blockType, name) {
			providerAddrs = providerAddressRanges(attr.Expr, map[hcl.Range]bool{})
		}
		var walkErr error
		hclsyntax.VisitAll(attr.Expr, func(node hclsyntax.Node) hcl.Diagnostics {
			if walkErr != nil {
				return nil
			}
			if providerAddrs != nil {
				// only the instance keys of provider references, such as
				// local.k in random.by_key[local.k], are values
				if expr, ok := node.(*hclsyntax.ScopeTraversalExpr); ok && !providerAddrs[expr.SrcRange] {
					walkErr = fn(expr.Traversal, inMoved)
				}
				return nil
			}
			if expr, ok := node.(*hclsyntax.ScopeTraversalExpr); ok {
				walkErr = fn(expr.Traversal, inMoved)
			} else if tr, ok := joinedTraversal(node); ok {
				walkErr = fn(tr, inMoved)
			}
			return nil
		})
		if walkErr != nil {
			return walkErr
		}
	}
	for _, block := range body.Blocks {
		if err := walkBodyTraversals(block.Body, block.Type, inMoved || block.Type == "moved", fn); err != nil {
			return err
		}
	}
	return nil
}

// joinedTraversal rebuilds the whole traversal of a splat such as
// module.x[*].out, or of an index with a dynamic key such as
// module.x[local.k].out, which the syntax splits into a source
// expression and a relative traversal. The key of a dynamic index is
// left unknown.
func joinedTraversal(node hclsyntax.Node) (hcl.Traversal, bool) {
	switch e := node.(type) {
	case *hclsyntax.SplatExpr:
		src, ok := e.Source.(*hclsyntax.ScopeTraversalExpr)
		if !ok {
			return nil, false
		}
		each, ok := e.Each.(*hclsyntax.RelativeTraversalExpr)
		if !ok {
			return nil, false
		}
		if _, ok := each.Source.(*hclsyntax.AnonSymbolExpr); !ok {
			return nil, false
		}
		tr := append(hcl.Traversal{}, src.Traversal...)
		tr = append(tr, hcl.TraverseSplat{SrcRange: e.MarkerRange})
		return append(tr, each.Traversal...), true
	case *hclsyntax.RelativeTraversalExpr:
		idx, ok := e.Source.(*hclsyntax.IndexExpr)
		if !ok {
			return nil, false
		}
		coll, ok := idx.Collection.(*hclsyntax.ScopeTraversalExpr)
		if !ok {
			return nil, false
		}
		tr := append(hcl.Traversal{}, coll.Traversal...)
		tr = append(tr, hcl.TraverseIndex{Key: cty.DynamicVal, SrcRange: idx.BracketRange})
		return append(tr, e.Traversal...), true
	}
	return nil, false
}

// providerAddressRanges collects the ranges of the provider references
// (such as local.secondary or random.by_key) and of the object keys in a
// provider or providers meta-argument, which look like values but are
// not.
func providerAddressRanges(expr hclsyntax.Expression, ranges map[hcl.Range]bool) map[hcl.Range]bool {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		ranges[e.SrcRange] = true
	case *hclsyntax.IndexExpr:
		providerAddressRanges(e.Collection, ranges)
	case *hclsyntax.ObjectConsExpr:
		for _, item := range e.Items {
			ranges[item.KeyExpr.Range()] = true
			providerAddressRanges(item.ValueExpr, ranges)
		}
	}
	return ranges
}

func isProviderMetaArgument(blockType, attrName string) bool {
	switch blockType {
	case "resource", "data", "ephemeral":
		return attrName == "provider"
	case "module":
		return attrName == "providers"
	}
	return false
}

// callsOfModule returns the names of the module calls in modPath
// whose local source is childPath.
func callsOfModule(env Env, modPath, childPath string) map[string]bool {
	names := map[string]bool{}
	if env.ModuleCalls == nil {
		return names
	}
	calls, err := env.ModuleCalls(modPath)
	if err != nil {
		return names
	}
	for name := range calls {
		if p, ok := localModuleCallPath(env, modPath, name); ok && p == filepath.Clean(childPath) {
			names[name] = true
		}
	}
	return names
}

// stepName is the bare name of a root or attribute step
// (AttrStep.String has a leading dot).
func stepName(step lang.AddressStep) string {
	switch s := step.(type) {
	case lang.RootStep:
		return s.Name
	case lang.AttrStep:
		return s.Name
	}
	return step.String()
}

func endOfFile(src []byte, filename string) hcl.Range {
	line, col := 1, 1
	for _, r := range string(src) {
		if r == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	pos := hcl.Pos{Line: line, Column: col, Byte: len(src)}
	return hcl.Range{Filename: filename, Start: pos, End: pos}
}

// movedBlock is the text appended to the end of a file, so that
// OpenTofu moves the existing state instead of destroying and
// recreating the infrastructure.
func movedBlock(src []byte, from, to lang.Address) string {
	prefix := "\n"
	if len(src) > 0 && src[len(src)-1] != '\n' {
		prefix = "\n\n"
	}
	block := fmt.Sprintf("%smoved {\n  from = %s\n  to   = %s\n}\n", prefix, from.String(), to.String())
	if bytes.Contains(src, []byte("\r\n")) {
		// keep the file's line endings
		block = strings.ReplaceAll(block, "\n", "\r\n")
	}
	return block
}

type editSet struct {
	edits map[string]map[hcl.Range]string
}

func newEditSet() *editSet {
	return &editSet{edits: map[string]map[hcl.Range]string{}}
}

func (s *editSet) add(file string, rng hcl.Range, text string) {
	if _, ok := s.edits[file]; !ok {
		s.edits[file] = map[hcl.Range]string{}
	}
	s.edits[file][rng] = text
}

func (s *editSet) list() []Edit {
	list := make([]Edit, 0)
	for file, edits := range s.edits {
		for rng, text := range edits {
			list = append(list, Edit{File: file, Range: rng, NewText: text})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].File != list[j].File {
			return list[i].File < list[j].File
		}
		return list[i].Range.Start.Byte < list[j].Range.Start.Byte
	})
	return list
}

// DeclarationNameRange returns the range of the declared name of a
// top-level declaration (a variable, local, output, resource, data
// source or module call), e.g. for highlighting it.
func DeclarationNameRange(env Env, path lang.Path, target reference.Target) (hcl.Range, bool) {
	kind, ok := kindOfTarget(target)
	if !ok || target.RangePtr == nil {
		return hcl.Range{}, false
	}
	sym, err := symbolFromTarget(newFileCache(env), path, kind, target)
	if err != nil {
		return hcl.Range{}, false
	}
	return sym.NameRange, true
}
