// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package refactor implements rename of OpenTofu symbols: variables,
// locals, outputs, resources, data sources and module calls.
package refactor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfmod "github.com/opentofu/opentofu-schema/module"
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
			return nil, nil, fmt.Errorf("%s has syntax errors; fix them before renaming", filepath.Base(path))
		}
		c.files[path] = f
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected body in %s", filepath.Base(path))
	}
	return body, src, nil
}

// FindSymbol returns the renamable symbol at pos, or ErrNotRenamable.
func FindSymbol(ctx context.Context, env Env, path lang.Path, file string, pos hcl.Pos) (*Symbol, error) {
	return findSymbol(ctx, env, newFileCache(env), path, file, pos)
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

	for _, target := range pathCtx.ReferenceTargets {
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
	for _, block := range body.Blocks {
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
	if _, exists := declaredTarget(pathCtx, sym.Kind, newAddr); exists {
		return nil, fmt.Errorf("%s already exists", newAddr.String())
	}

	fc := newFileCache(env)
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
				prefix := []string{"module", call, sym.Name}
				err := walkModuleTraversals(fc, p.Path, parentCtx, func(file string, tr hcl.Traversal, _ bool) error {
					if rng, ok := stepRangeIfPrefix(tr, prefix, 2); ok {
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
	filenames := make([]string, 0, len(pathCtx.Files))
	for name := range pathCtx.Files {
		if strings.HasSuffix(name, ".json") {
			continue
		}
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)

	for _, name := range filenames {
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

func walkBodyTraversals(body *hclsyntax.Body, blockType string, inMoved bool, fn func(tr hcl.Traversal, inMoved bool) error) error {
	for name, attr := range body.Attributes {
		if isProviderMetaArgument(blockType, name) {
			continue
		}
		var walkErr error
		hclsyntax.VisitAll(attr.Expr, func(node hclsyntax.Node) hcl.Diagnostics {
			if walkErr != nil {
				return nil
			}
			if expr, ok := node.(*hclsyntax.ScopeTraversalExpr); ok {
				walkErr = fn(expr.Traversal, inMoved)
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
	return fmt.Sprintf("%smoved {\n  from = %s\n  to   = %s\n}\n", prefix, from.String(), to.String())
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
