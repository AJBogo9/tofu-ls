// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"
	"sort"
	"strings"

	hclschema "github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/tofu/datadir"
)

// resourceTypes reports resource and data source types which the loaded
// schema of their provider does not have.
//
// It stays silent unless the provider resolves (from the provider
// argument, else the type's prefix, through required_providers or as an
// implied hashicorp provider), the module's lock file pins its version,
// and a schema of exactly that version is loaded: the one tofu init
// installed, or the bundled one when its version is the locked one.
// terraform_data and terraform_remote_state are built in.
func (c *checker) resourceTypes() {
	for _, d := range c.idx.decls {
		if d.kind != "resource" && d.kind != "data" {
			continue
		}
		if d.override || d.shadowed || c.mod.Broken[d.file] || c.idx.overridden[d.address] {
			continue
		}
		localName := ""
		meta, _, _ := d.block.Body.PartialContent(metaArgsSchema)
		if meta != nil {
			if attr, ok := meta.Attributes["provider"]; ok {
				tr, diags := hcl.AbsTraversalForExpr(attr.Expr)
				if diags.HasErrors() {
					continue
				}
				localName = tr.RootName()
			}
		}
		if localName == "" {
			localName = impliedProviderName(d.typ)
		}
		addr, schema := c.schemaFor(localName)
		if schema == nil {
			continue
		}
		types := schema.Resources
		if d.kind == "data" {
			types = schema.DataSources
		}
		if _, ok := types[d.typ]; ok {
			continue
		}
		c.unknownType(d.kind, d.typ, addr, types, d.block.LabelRanges[0])
	}
}

// unknownTypeRef reports a reference whose resource type the loaded
// schema does not have, and whose target no block declares. It returns
// false when the type's schema is not known.
func (c *checker) unknownTypeRef(tr hcl.Traversal, kind, typ string, rng hcl.Range) bool {
	addr, schema := c.schemaFor(impliedProviderName(typ))
	if schema == nil {
		return false
	}
	types := schema.Resources
	if kind == "data" {
		types = schema.DataSources
	}
	if _, ok := types[typ]; ok {
		return false
	}
	c.unknownType(kind, typ, addr, types, rng)
	return true
}

func (c *checker) unknownType(kind, typ string, addr tfaddr.Provider, types map[string]*hclschema.BodySchema, rng hcl.Range) {
	what := "resource type"
	if kind == "data" {
		what = "data source"
	}
	detail := fmt.Sprintf("The provider %s does not support %s %q.", addr.ForDisplay(), what, typ)
	if suggestion := nameSuggestion(typ, types); suggestion != "" {
		detail += fmt.Sprintf(" Did you mean %q?", suggestion)
	}
	summary := "Invalid resource type"
	if kind == "data" {
		summary = "Invalid data source"
	}
	c.add(hcl.DiagError, summary, detail, rng, ilsp.CodedDiagnostic{
		Code: ilsp.CodeUnknownResourceType,
		Data: map[string]interface{}{
			"type":     typ,
			"kind":     kind,
			"provider": addr.String(),
		},
	})
}

// impliedProviderName is the local provider name OpenTofu implies from a
// resource type: the part before the first underscore.
func impliedProviderName(typ string) string {
	if i := strings.IndexByte(typ, '_'); i > 0 {
		return typ[:i]
	}
	return typ
}

// schemaFor returns a provider's address and its schema for the version
// the module's lock file pins, when that schema is loaded.
func (c *checker) schemaFor(localName string) (tfaddr.Provider, *tfschema.ProviderSchema) {
	if c.mod.Schema == nil || localName == "terraform" {
		return tfaddr.Provider{}, nil
	}
	addr, ok := c.providerAddr(localName)
	if !ok {
		return tfaddr.Provider{}, nil
	}
	lock := c.lockFile()
	v, ok := lock[addr]
	if !ok || v == nil {
		return tfaddr.Provider{}, nil
	}
	if schema, ok := c.schemas[addr]; ok {
		return addr, schema
	}
	schema := c.mod.Schema(addr, v)
	c.schemas[addr] = schema
	return addr, schema
}

// lockFile returns the provider versions locked for the module, read
// once per run.
func (c *checker) lockFile() datadir.PluginVersionMap {
	if !c.lockSet {
		c.lockSet = true
		if c.mod.FS != nil {
			c.lock, _ = datadir.ParsePluginVersions(c.mod.FS, c.mod.Path)
		}
	}
	return c.lock
}

// nameSuggestion returns the closest name to given, like OpenTofu's
// "Did you mean" (an edit distance under 3), or "".
func nameSuggestion(given string, names map[string]*hclschema.BodySchema) string {
	candidates := make([]string, 0, len(names))
	for name := range names {
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	best, bestDist := "", 3
	for _, name := range candidates {
		if d := levenshtein(given, name); d < bestDist {
			best, bestDist = name, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
