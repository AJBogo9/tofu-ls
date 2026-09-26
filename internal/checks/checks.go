// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package checks finds the errors OpenTofu reports at validate time
// (duplicate declarations, unresolved references, invalid types, values
// of the wrong type, missing installations) while the configuration is
// being edited.
//
// Every check follows the same rule: a diagnostic is reported only when
// it is certain. Whatever cannot be known statically (an unknown value,
// a schema which is not loaded, a file which does not parse) makes the
// check stay silent.
package checks

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	tfmod "github.com/opentofu/opentofu-schema/module"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/settings"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// FS is the part of the language server's filesystem the checks read:
// the lock file, the module manifest, installed providers and local
// module directories.
type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
}

// SchemaReader returns the schema of a provider loaded for exactly the
// given (locked) version, or nil when none is loaded.
type SchemaReader func(addr tfaddr.Provider, v *version.Version) *tfschema.ProviderSchema

// Module is what the checks know about one module directory.
type Module struct {
	// Path is the module's directory.
	Path string

	// Files are the module's parsed configuration files (.tf, .tofu,
	// .tf.json and .tofu.json) by base name.
	Files map[string]*hcl.File

	// Broken names the files with syntax errors. Checks which need every
	// declaration of the module are skipped while any file is broken, and
	// no check looks inside a broken file.
	Broken map[string]bool

	// VarsFiles are the .tfvars files of the directory by base name, and
	// BrokenVars those with syntax errors.
	VarsFiles  map[string]*hcl.File
	BrokenVars map[string]bool

	// Meta is the early decoding of the module (variable types, provider
	// requirements, module calls).
	Meta *tfmod.Meta

	// ChildOutputs maps a module call name to the outputs of the module
	// it calls, for calls whose module is indexed and parsed cleanly.
	ChildOutputs map[string]map[string]bool

	// Schema returns a provider's loaded schema for a locked version.
	Schema SchemaReader

	FS FS

	// Values evaluates the module's values without a plan, with the
	// inputs chosen for it and its callers (see package staticval). The
	// condition checks are skipped without it.
	Values *staticval.Evaluator
	// ValuesEnv resolves and loads the modules that the module calls, so
	// that their validation rules check the arguments.
	ValuesEnv staticval.Env
}

// Run runs the checks enabled in opts and returns their diagnostics by
// file base name.
func Run(mod *Module, opts settings.ValidationOptions) map[string]hcl.Diagnostics {
	c := newChecker(mod)

	if opts.DuplicateDeclarations {
		c.duplicates()
	}
	if opts.UnresolvedReferences || opts.UnknownResourceTypes {
		c.references(opts.UnresolvedReferences, opts.UnknownResourceTypes)
	}
	if opts.UnknownResourceTypes {
		c.resourceTypes()
	}
	if opts.VariableTypes {
		c.variableTypes()
	}
	if opts.Tfvars {
		c.tfvars()
	}
	if opts.StaticValues {
		c.staticValues()
	}
	if opts.OperandTypes {
		c.operands()
	}
	if opts.Installation {
		c.installation()
	}
	if opts.UnusedDataSources {
		c.unusedDataSources()
	}
	if opts.InterpolationOnly {
		c.interpolationOnly()
	}
	if opts.Conditions {
		c.conditions()
	}

	return c.diags
}

type checker struct {
	mod   *Module
	diags map[string]hcl.Diagnostics

	// names lists the configuration files in the order OpenTofu reads
	// them (lexical).
	names []string
	// complete is true when every file parsed cleanly.
	complete bool

	idx *index

	lock    map[tfaddr.Provider]*version.Version
	lockSet bool

	// schemas caches the provider schemas looked up during a run
	schemas map[tfaddr.Provider]*tfschema.ProviderSchema
}

func newChecker(mod *Module) *checker {
	c := &checker{
		mod:      mod,
		diags:    make(map[string]hcl.Diagnostics),
		complete: len(mod.Files) > 0,
		schemas:  make(map[tfaddr.Provider]*tfschema.ProviderSchema),
	}
	for name := range mod.Files {
		c.names = append(c.names, name)
		if mod.Broken[name] || mod.Files[name] == nil {
			c.complete = false
		}
	}
	sort.Strings(c.names)
	c.idx = buildIndex(c)
	return c
}

func (c *checker) add(sev hcl.DiagnosticSeverity, summary, detail string, rng hcl.Range, extra ilsp.CodedDiagnostic) {
	name := rng.Filename
	c.diags[name] = append(c.diags[name], &hcl.Diagnostic{
		Severity: sev,
		Summary:  summary,
		Detail:   detail,
		Subject:  rng.Ptr(),
		Extra:    extra,
	})
}

// fileURI returns the URI of a file of the module.
func (c *checker) fileURI(name string) string {
	return uri.FromPath(filepath.Join(c.mod.Path, name))
}

// usable reports whether the checks look inside a file: it parsed, and
// OpenTofu reads it (a .tf file next to a .tofu file of the same name is
// ignored).
func (c *checker) usable(name string) bool {
	return c.mod.Files[name] != nil && !c.mod.Broken[name] && !c.shadowed(name)
}

// shadowed reports whether OpenTofu ignores a file because a .tofu file
// of the same name exists (and likewise for the JSON forms).
func (c *checker) shadowed(name string) bool {
	for _, pair := range [][2]string{{".tf.json", ".tofu.json"}, {".tf", ".tofu"}} {
		if strings.HasSuffix(name, pair[0]) {
			_, ok := c.mod.Files[strings.TrimSuffix(name, pair[0])+pair[1]]
			return ok
		}
	}
	return false
}

// isOverrideFile reports whether a configuration file is an override
// file (override.tf, *_override.tf and their .tofu and JSON forms),
// whose blocks OpenTofu merges into the primary declarations.
func isOverrideFile(name string) bool {
	for _, ext := range []string{".tf.json", ".tofu.json", ".tf", ".tofu"} {
		if strings.HasSuffix(name, ext) {
			base := strings.TrimSuffix(name, ext)
			return base == "override" || strings.HasSuffix(base, "_override")
		}
	}
	return false
}

func isJSON(name string) bool {
	return strings.HasSuffix(name, ".json")
}

// labelsRange spans a block's labels, or its type when it has none.
func labelsRange(b *hcl.Block) hcl.Range {
	if len(b.LabelRanges) == 0 {
		return b.TypeRange
	}
	return hcl.RangeBetween(b.LabelRanges[0], b.LabelRanges[len(b.LabelRanges)-1])
}

// providerAddr resolves a local provider name as OpenTofu does: through
// required_providers, else as an implied hashicorp provider. The second
// result is false for the built-in terraform provider and for names the
// early decoder did not see.
func (c *checker) providerAddr(localName string) (tfaddr.Provider, bool) {
	if c.mod.Meta == nil {
		return tfaddr.Provider{}, false
	}
	addr, ok := c.mod.Meta.ProviderReferences[tfmod.ProviderRef{LocalName: localName}]
	if !ok {
		return tfaddr.Provider{}, false
	}
	return normalizeProvider(addr)
}

func normalizeProvider(addr tfaddr.Provider) (tfaddr.Provider, bool) {
	if addr.IsBuiltIn() || addr.Type == "terraform" && (addr.IsLegacy() || addr.Namespace == "hashicorp") {
		return tfaddr.Provider{}, false
	}
	if addr.IsLegacy() {
		addr.Namespace = "hashicorp"
	}
	return addr, true
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
