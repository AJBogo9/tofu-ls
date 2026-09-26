// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl-lang/validator"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfmod "github.com/opentofu/opentofu-schema/module"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	"github.com/opentofu/tofu-ls/internal/features/modules/decoder/validations"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/zclconf/go-cty/cty"
)

// read-only and shared by all test path contexts
var (
	testSemanticHighlighting = tfschema.TestSemanticHighlighting()
	staticTestSchema         = tfschema.TestFileSchema(tfschema.LatestAvailableVersion)
	mockSchema               = tfschema.MockFileSchema()
)

var testValidators = []validator.Validator{
	validator.BlockLabelsLength{},
	validator.MaxBlocks{},
	// skips top-level provider blocks, whose required arguments may be
	// set by the environment
	validations.MissingRequiredAttribute{},
	validator.UnexpectedAttribute{},
	validator.UnexpectedBlock{},
}

// filesOf returns the test files (mock false) or the mock data files
// (mock true) of a record.
func filesOf(record *state.TestRecord, mock bool) map[string]*hcl.File {
	files := make(map[string]*hcl.File)
	for name, f := range record.ParsedFiles {
		if name.IsMock() == mock {
			files[name.String()] = f
		}
	}
	return files
}

func staticTestPathContext(record *state.TestRecord) *decoder.PathContext {
	return &decoder.PathContext{
		Schema:               staticTestSchema,
		ReferenceOrigins:     make(reference.Origins, 0),
		ReferenceTargets:     make(reference.Targets, 0),
		Files:                filesOf(record, false),
		Validators:           testValidators,
		SemanticHighlighting: testSemanticHighlighting,
	}
}

func mockPathContext(record *state.TestRecord) *decoder.PathContext {
	files := filesOf(record, true)
	pathCtx := &decoder.PathContext{
		Schema:               mockSchema,
		ReferenceOrigins:     make(reference.Origins, 0),
		ReferenceTargets:     make(reference.Targets, 0),
		Files:                files,
		Validators:           testValidators,
		SemanticHighlighting: testSemanticHighlighting,
	}
	for _, origin := range record.RefOrigins {
		if _, ok := files[origin.OriginRange().Filename]; ok {
			pathCtx.ReferenceOrigins = append(pathCtx.ReferenceOrigins, origin)
		}
	}
	return pathCtx
}

// testPathContext returns the context of the test files of a directory.
// Their references resolve against the modules the runs run: assertions,
// expect_failures, plan options and overrides of a run refer to the
// objects of its module, and var.<name> elsewhere to the root module's
// inputs, where tofu test runs. Such origins become path origins to the
// module, and copies of the module's targets, which are targetable only
// where they are in scope, serve hover, completion and highlighting.
func testPathContext(record *state.TestRecord, modules ModuleReader, withSchema bool) *decoder.PathContext {
	files := filesOf(record, false)
	pathCtx := &decoder.PathContext{
		ReferenceOrigins:     make(reference.Origins, 0),
		ReferenceTargets:     make(reference.Targets, 0),
		Files:                files,
		Validators:           testValidators,
		SemanticHighlighting: testSemanticHighlighting,
	}

	isModule := func(dir string) bool {
		_, err := modules.LocalModuleMeta(dir)
		return err == nil
	}
	scope := newTestScope(files, ast.ModuleRoot(record.Path(), isModule))
	metas := make(map[string]*tfmod.Meta)
	for _, dir := range scope.modules() {
		if meta, err := modules.LocalModuleMeta(dir); err == nil {
			metas[dir] = meta
		}
	}

	if withSchema {
		pathCtx.Schema = testSchema(scope, metas, modules, pathCtx)
	}

	pathCtx.ReferenceTargets = testTargets(record, scope, metas, modules)
	pathCtx.ReferenceOrigins = testOrigins(record, scope, metas)
	if !withSchema {
		pathCtx.ReferenceOriginIndex = reference.NewOriginIndex(pathCtx.ReferenceOrigins)
	}

	return pathCtx
}

// testSchema returns the schema of the test files: provider blocks take
// the root module's provider schema, and variables blocks the inputs of
// the module they set. It also sets the root module's functions.
func testSchema(scope *testScope, metas map[string]*tfmod.Meta, modules ModuleReader, pathCtx *decoder.PathContext) *schema.BodySchema {
	bs := *staticTestSchema
	bs.Blocks = make(map[string]*schema.BlockSchema, len(staticTestSchema.Blocks))
	for name, block := range staticTestSchema.Blocks {
		bs.Blocks[name] = block
	}

	rootCtx, err := modules.PathContext(lang.Path{Path: scope.root, LanguageID: ilsp.OpenTofu.String()})
	if err == nil {
		pathCtx.Functions = rootCtx.Functions
		if rootCtx.Schema != nil {
			if provider, ok := rootCtx.Schema.Blocks["provider"]; ok {
				bs.Blocks["provider"] = provider
			}
		}
	}

	if meta, ok := metas[scope.root]; ok {
		bs.Blocks["variables"] = tfschema.TestVariablesBlockSchema(meta.Variables, false)
	}

	// every run has a body of its own, or else a failed lookup would
	// leave its body unvalidated
	run := *staticTestSchema.Blocks["run"]
	run.DependentBody = make(map[schema.SchemaKey]*schema.BodySchema)
	for _, fileRuns := range scope.runs {
		for _, r := range fileRuns {
			var vars map[string]tfmod.Variable
			if meta, ok := metas[r.Module]; ok {
				vars = meta.Variables
			}
			key, body := tfschema.TestRunDependentBody(r.Name, vars)
			run.DependentBody[key] = body
		}
	}
	bs.Blocks["run"] = &run

	return &bs
}

// testTargets returns the targets the test files declare (providers),
// the outputs of runs, and copies of the targets of the modules the
// runs run, each targetable where it is in scope.
func testTargets(record *state.TestRecord, scope *testScope, metas map[string]*tfmod.Meta, modules ModuleReader) reference.Targets {
	targets := make(reference.Targets, 0)
	for _, target := range record.RefTargets {
		if target.RangePtr != nil && scope.files[target.RangePtr.Filename] != nil {
			targets = append(targets, target)
		}
	}

	for _, dir := range scope.modules() {
		meta, ok := metas[dir]
		if !ok {
			continue
		}
		note := scope.moduleNote(dir)
		bodies := scope.runBodies[dir]

		varRegions := bodies
		objectRegions := bodies
		if dir == scope.root {
			varRegions = append(append([]hcl.Range{}, bodies...), scope.global...)
			objectRegions = append(append([]hcl.Range{}, bodies...), scope.overrides...)
		}

		targets = append(targets, targetableFrom(tfschema.TestVariableTargets(meta.Variables, note), varRegions)...)
		targets = append(targets, targetableFrom(tfschema.TestOutputTargets(meta.Outputs, note), bodies)...)

		if len(objectRegions) == 0 {
			continue
		}
		refCtx, err := modules.ReferencePathContext(lang.Path{Path: dir, LanguageID: ilsp.OpenTofu.String()})
		if err != nil {
			continue
		}
		for _, target := range refCtx.ReferenceTargets {
			if len(target.Addr) == 0 {
				// e.g. each.value, only targetable inside its block
				continue
			}
			if root := target.Addr[0].String(); root == "var" || root == "output" {
				// declared above, with their descriptions
				continue
			}
			targets = append(targets, targetableFrom(reference.Targets{target}, objectRegions)...)
		}
	}

	// run.<name>.<output>, in the variables blocks of the later runs of
	// the same file
	for _, filename := range scope.filenames() {
		runs := scope.runs[filename]
		for i, r := range runs {
			if r.Name == "" {
				continue
			}
			later := make([]hcl.Range, 0)
			for _, next := range runs[i+1:] {
				later = append(later, variablesBlockRanges(next.Block.Body)...)
			}
			if len(later) == 0 {
				continue
			}
			var outputs map[string]tfmod.Output
			if meta, ok := metas[r.Module]; ok {
				outputs = meta.Outputs
			}
			target := tfschema.TestRunTarget(r.Name, outputs, r.Block.Range(), r.Block.DefRange(), scope.runNote(r))
			if outputs == nil {
				// a module that is not indexed may have any output
				target.Type = cty.DynamicPseudoType
			}
			targets = append(targets, targetableFrom(reference.Targets{target}, later)...)
		}
	}

	return targets
}

// targetableFrom returns copies of targets (and their nested targets)
// which are targetable from the regions only. The copies of module
// targets drop their ranges, which are in the module's files: path
// origins resolve them in the module instead. No regions means that the
// targets are not in scope anywhere.
func targetableFrom(targets reference.Targets, regions []hcl.Range) reference.Targets {
	if len(regions) == 0 {
		return nil
	}
	copies := make(reference.Targets, 0, len(targets))
	for _, target := range targets {
		copies = append(copies, withRegions(target, regions))
	}
	return copies
}

func withRegions(target reference.Target, regions []hcl.Range) reference.Target {
	t := target.Copy()
	t.TargetableFromRanges = regions
	if t.ScopeId != tfschema.RunScope {
		t.RangePtr = nil
		t.DefRangePtr = nil
	}
	for i, nested := range t.NestedTargets {
		t.NestedTargets[i] = withRegions(nested, regions)
	}
	return t
}

// testOrigins returns the origins of the test files, those which refer to
// a module's objects turned into path origins to that module.
func testOrigins(record *state.TestRecord, scope *testScope, metas map[string]*tfmod.Meta) reference.Origins {
	origins := make(reference.Origins, 0, len(record.RefOrigins))

	declares := func(dir, name string) bool {
		meta, ok := metas[dir]
		if !ok {
			return false
		}
		_, ok = meta.Variables[name]
		return ok
	}
	toModule := func(lo reference.LocalOrigin, dir string, cons reference.OriginConstraints) reference.Origin {
		return reference.PathOrigin{
			Range:       lo.Range,
			TargetAddr:  lo.Addr,
			TargetPath:  lang.Path{Path: dir, LanguageID: ilsp.OpenTofu.String()},
			Constraints: cons,
		}
	}

	for _, origin := range record.RefOrigins {
		rng := origin.OriginRange()
		if scope.files[rng.Filename] == nil {
			continue
		}
		lo, ok := origin.(reference.LocalOrigin)
		if !ok || len(lo.Addr) < 2 || constrainedTo(lo.Constraints, tfschema.ProviderScope) {
			origins = append(origins, origin)
			continue
		}

		kind, dir := scope.contextAt(rng)
		name := ""
		if step, ok := lo.Addr[1].(lang.AttrStep); ok {
			name = step.Name
		}
		switch lo.Addr[0].String() {
		case "run":
			origins = append(origins, lo)
		case "var":
			if kind != runBody {
				dir = scope.root
			}
			if dir != "" && declares(dir, name) {
				origins = append(origins, toModule(lo, dir, lo.Constraints))
			} else {
				origins = append(origins, lo)
			}
		case "output":
			// outputs are type-less targets in the module
			if kind == runBody && metas[dir] != nil {
				origins = append(origins, toModule(lo, dir, reference.OriginConstraints{{OfScopeId: tfschema.OutputScope}}))
			} else {
				origins = append(origins, lo)
			}
		default:
			switch {
			case kind == runBody && metas[dir] != nil:
				origins = append(origins, toModule(lo, dir, lo.Constraints))
			case kind == fileOverride && metas[scope.root] != nil:
				origins = append(origins, toModule(lo, scope.root, lo.Constraints))
			default:
				origins = append(origins, lo)
			}
		}
	}

	// the names in variables blocks, like the keys of a tfvars file
	for _, filename := range scope.filenames() {
		body := scope.bodies[filename]
		keyOrigins := func(block *hclsyntax.Block, dir string) {
			for _, attr := range block.Body.Attributes {
				if dir == "" || !declares(dir, attr.Name) {
					continue
				}
				origins = append(origins, reference.PathOrigin{
					Range:      attr.NameRange,
					TargetAddr: lang.Address{lang.RootStep{Name: "var"}, lang.AttrStep{Name: attr.Name}},
					TargetPath: lang.Path{Path: dir, LanguageID: ilsp.OpenTofu.String()},
					Constraints: reference.OriginConstraints{{
						OfScopeId: tfschema.VariableScope,
						OfType:    cty.DynamicPseudoType,
					}},
				})
			}
		}
		for _, block := range body.Blocks {
			if block.Type == "variables" {
				keyOrigins(block, scope.root)
			}
		}
		for _, r := range scope.runs[filename] {
			for _, block := range r.Block.Body.Blocks {
				if block.Type == "variables" {
					keyOrigins(block, r.Module)
				}
			}
		}
	}

	sort.SliceStable(origins, func(i, j int) bool {
		if origins[i].OriginRange().Filename != origins[j].OriginRange().Filename {
			return origins[i].OriginRange().Filename < origins[j].OriginRange().Filename
		}
		return origins[i].OriginRange().Start.Byte < origins[j].OriginRange().Start.Byte
	})

	return origins
}

func constrainedTo(cons reference.OriginConstraints, scope lang.ScopeId) bool {
	for _, c := range cons {
		if c.OfScopeId == scope {
			return true
		}
	}
	return false
}

// contextKind tells which module an expression of a test file refers to.
type contextKind int

const (
	// fileGlobal: variables, provider and mock_provider blocks of the
	// file, where var.<name> is the root module's input
	fileGlobal contextKind = iota
	// fileOverride: override blocks of the file, whose targets are in
	// the root module
	fileOverride
	// runGlobal: the variables and module blocks of a run, like fileGlobal
	runGlobal
	// runBody: anything else in a run, which refers to the run's module
	runBody
)

// testScope knows the runs of the test files of a directory, the module
// each one runs, and the regions of the files where each module is in
// scope.
type testScope struct {
	root   string
	files  map[string]*hcl.File
	bodies map[string]*hclsyntax.Body
	runs   map[string][]ast.Run

	// runBodies are the regions of the runs of each module which refer
	// to its objects (assertions, expect_failures, plan_options and
	// overrides)
	runBodies map[string][]hcl.Range
	// global are the regions where var.<name> is the root's input
	global []hcl.Range
	// overrides are the override blocks of the files
	overrides []hcl.Range
}

func newTestScope(files map[string]*hcl.File, root string) *testScope {
	s := &testScope{
		root:      root,
		files:     files,
		bodies:    make(map[string]*hclsyntax.Body),
		runs:      make(map[string][]ast.Run),
		runBodies: make(map[string][]hcl.Range),
	}
	for name, f := range files {
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		s.bodies[name] = body
		s.runs[name] = ast.Runs(body, root)

		for _, block := range body.Blocks {
			switch block.Type {
			case "variables", "provider", "mock_provider":
				s.global = append(s.global, block.Range())
			case "override_resource", "override_data", "override_module":
				s.overrides = append(s.overrides, block.Range())
			}
		}
		for _, r := range s.runs[name] {
			for _, attr := range r.Block.Body.Attributes {
				s.addRunBody(r.Module, attr.SrcRange)
			}
			for _, nested := range r.Block.Body.Blocks {
				if nested.Type == "variables" || nested.Type == "module" {
					s.global = append(s.global, nested.Range())
					continue
				}
				s.addRunBody(r.Module, nested.Range())
			}
		}
	}
	return s
}

func (s *testScope) addRunBody(dir string, rng hcl.Range) {
	if dir == "" {
		return
	}
	s.runBodies[dir] = append(s.runBodies[dir], rng)
}

// modules returns the root and the local modules which runs run.
func (s *testScope) modules() []string {
	dirs := []string{s.root}
	for dir := range s.runBodies {
		if dir != s.root {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs[1:])
	return dirs
}

func (s *testScope) filenames() []string {
	names := make([]string, 0, len(s.bodies))
	for name := range s.bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// contextAt returns the kind of context of a range in a test file, and
// for a run's body the module of the run ("" when it is not local).
func (s *testScope) contextAt(rng hcl.Range) (contextKind, string) {
	body, ok := s.bodies[rng.Filename]
	if !ok {
		return fileGlobal, ""
	}
	for _, r := range s.runs[rng.Filename] {
		if !r.Block.Range().ContainsPos(rng.Start) {
			continue
		}
		for _, nested := range r.Block.Body.Blocks {
			if (nested.Type == "variables" || nested.Type == "module") && nested.Range().ContainsPos(rng.Start) {
				return runGlobal, r.Module
			}
		}
		return runBody, r.Module
	}
	for _, block := range body.Blocks {
		switch block.Type {
		case "override_resource", "override_data", "override_module":
			if block.Range().ContainsPos(rng.Start) {
				return fileOverride, s.root
			}
		}
	}
	return fileGlobal, s.root
}

func (s *testScope) relPath(dir string) string {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil {
		return dir
	}
	rel = filepath.ToSlash(rel)
	if rel != "." && !filepath.IsAbs(rel) && rel[0] != '.' {
		rel = "./" + rel
	}
	return rel
}

func (s *testScope) moduleNote(dir string) string {
	if dir == s.root {
		return "Declared in the module under test (the root module, where tofu test runs)."
	}
	return fmt.Sprintf("Declared in module `%s`, which a run of this file runs.", s.relPath(dir))
}

func (s *testScope) runNote(r ast.Run) string {
	switch r.Module {
	case "":
		return fmt.Sprintf("Run %q runs a module that is not local; its outputs are not known here.", r.Name)
	case s.root:
		return fmt.Sprintf("Run %q runs the root module.", r.Name)
	}
	return fmt.Sprintf("Run %q runs module `%s`.", r.Name, s.relPath(r.Module))
}

func variablesBlockRanges(body *hclsyntax.Body) []hcl.Range {
	ranges := make([]hcl.Range, 0)
	for _, block := range body.Blocks {
		if block.Type == "variables" {
			ranges = append(ranges, block.Range())
		}
	}
	return ranges
}
