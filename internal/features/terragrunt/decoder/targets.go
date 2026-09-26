// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/paths"
	"github.com/zclconf/go-cty/cty"
)

// maxSourceText is the longest expression source shown in a hover.
const maxSourceText = 120

// sourceText returns the source of expr on one line, shortened.
func sourceText(expr hcl.Expression, src []byte) string {
	rng := expr.Range()
	if rng.End.Byte > len(src) || rng.Start.Byte > rng.End.Byte {
		return ""
	}
	text := strings.Join(strings.Fields(string(src[rng.Start.Byte:rng.End.Byte])), " ")
	if len(text) > maxSourceText {
		text = text[:maxSourceText] + "…"
	}
	return text
}

func codeSpan(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func blockTarget(root string, block *hclsyntax.Block, scope lang.ScopeId, name, desc string) reference.Target {
	return reference.Target{
		Addr: lang.Address{
			lang.RootStep{Name: root},
			lang.AttrStep{Name: block.Labels[0]},
		},
		ScopeId:     scope,
		RangePtr:    block.Range().Ptr(),
		DefRangePtr: block.DefRange().Ptr(),
		// what these hold is only known when Terragrunt runs, so any
		// reference under them resolves to the block
		Type:        cty.DynamicPseudoType,
		Name:        name,
		Description: lang.Markdown(desc),
	}
}

// FileTargets returns the targets which a Terragrunt file declares
// besides its local values (which the schema describes): its
// dependencies and feature flags, and the units and stacks of a stack
// file.
func FileTargets(file *hcl.File) reference.Targets {
	targets := make(reference.Targets, 0)
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return targets
	}
	attrText := func(block *hclsyntax.Block, name string) (string, bool) {
		attr, ok := block.Body.Attributes[name]
		if !ok {
			return "", false
		}
		return sourceText(attr.Expr, file.Bytes), true
	}

	for _, block := range body.Blocks {
		if len(block.Labels) != 1 {
			continue
		}
		name := block.Labels[0]
		switch block.Type {
		case "dependency":
			desc := fmt.Sprintf("Dependency `%s`", name)
			if text, ok := attrText(block, "config_path"); ok {
				desc += " on " + codeSpan(text)
			}
			desc += ". `outputs` holds its outputs, which Terragrunt reads from its state when it runs, or " +
				"`mock_outputs` while it has none."
			targets = append(targets, blockTarget("dependency", block, tfschema.TerragruntDependencyScope, "dependency", desc))
		case "feature":
			desc := fmt.Sprintf("Feature flag `%s`: `feature.%s.value` is", name, name)
			if text, ok := attrText(block, "default"); ok {
				desc += " " + codeSpan(text) + " (its default)"
			} else {
				desc += " its default"
			}
			desc += fmt.Sprintf(" unless `--feature %s=<value>` sets it.", name)
			targets = append(targets, blockTarget("feature", block, tfschema.TerragruntFeatureScope, "feature flag", desc))
		case "unit", "stack":
			scope := tfschema.TerragruntUnitScope
			if block.Type == "stack" {
				scope = tfschema.TerragruntStackScope
			}
			desc := fmt.Sprintf("The %s `%s` of this stack", block.Type, name)
			if text, ok := attrText(block, "source"); ok {
				desc += ", from " + codeSpan(text)
			}
			desc += fmt.Sprintf(". `%s.%s.path` is the directory it is generated in", block.Type, name)
			if text, ok := attrText(block, "path"); ok {
				desc += " (" + codeSpan(text) + ")"
			}
			desc += "."
			targets = append(targets, blockTarget(block.Type, block, scope, block.Type, desc))
		}
	}
	return targets
}

// IncludeCache keeps the included files parsed, as long as their
// contents stay the same.
type IncludeCache struct {
	mu    sync.Mutex
	files map[string]*hcl.File
}

func NewIncludeCache() *IncludeCache {
	return &IncludeCache{files: make(map[string]*hcl.File)}
}

func (c *IncludeCache) parse(path string, src []byte) *hcl.File {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if f, ok := c.files[path]; ok && bytes.Equal(f.Bytes, src) {
			return f
		}
	}
	f, _ := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if c != nil && f != nil {
		c.files[path] = f
	}
	return f
}

// IncludeTargets returns the targets of the exposed includes of the
// entry files (a unit's terragrunt.hcl) among files: include.<name>, and
// include.<name>.locals.<local> for the local values of the included
// file. Only includes whose path is static are read. The targets' ranges
// are in the included file, named relative to dir.
func IncludeTargets(fsys paths.FS, cache *IncludeCache, dir string, files map[string]*hcl.File) reference.Targets {
	targets := make(reference.Targets, 0)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		file := files[name]
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok || !ast.IsEntryFilename(name) {
			continue
		}
		resolver := paths.NewResolver(fsys, dir, file, true)
		for _, block := range body.Blocks {
			if block.Type != "include" || len(block.Labels) != 1 {
				continue
			}
			if expose, ok := block.Body.Attributes["expose"]; !ok {
				continue
			} else if exposed, ok := resolver.Bool(expose.Expr); !ok || !exposed {
				continue
			}
			pathAttr, ok := block.Body.Attributes["path"]
			if !ok {
				continue
			}
			includedPath, ok := resolver.Path(pathAttr.Expr)
			if !ok {
				continue
			}
			if _, ok := resolver.File(includedPath); !ok {
				continue
			}
			src, err := fsys.ReadFile(includedPath)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(dir, includedPath)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			included := cache.parse(includedPath, src)
			if included == nil {
				continue
			}
			targets = append(targets, includeTarget(block, rel, included))
		}
	}
	return targets
}

func includeTarget(block *hclsyntax.Block, rel string, included *hcl.File) reference.Target {
	name := block.Labels[0]
	target := reference.Target{
		Addr: lang.Address{
			lang.RootStep{Name: "include"},
			lang.AttrStep{Name: name},
		},
		ScopeId:     tfschema.TerragruntIncludeScope,
		RangePtr:    block.Range().Ptr(),
		DefRangePtr: block.DefRange().Ptr(),
		Type:        cty.DynamicPseudoType,
		Name:        "include",
		Description: lang.Markdown(fmt.Sprintf("The configuration in `%s`, included as `%s` with "+
			"`expose = true`: its blocks and attributes, such as `include.%s.locals` and `include.%s.inputs`.",
			rel, name, name, name)),
	}

	body, ok := included.Body.(*hclsyntax.Body)
	if !ok {
		return target
	}
	localsAddr := append(target.Addr.Copy(), lang.AttrStep{Name: "locals"})
	var localsTarget *reference.Target
	for _, block := range body.Blocks {
		if block.Type != "locals" {
			continue
		}
		if localsTarget == nil {
			localsTarget = &reference.Target{
				Addr:        localsAddr,
				ScopeId:     tfschema.TerragruntIncludeScope,
				RangePtr:    withFilename(block.Range(), rel).Ptr(),
				DefRangePtr: withFilename(block.DefRange(), rel).Ptr(),
				Name:        "local values",
				Description: lang.Markdown(fmt.Sprintf("The local values of `%s`.", rel)),
			}
		}
		attrs := make([]*hclsyntax.Attribute, 0, len(block.Body.Attributes))
		for _, attr := range block.Body.Attributes {
			attrs = append(attrs, attr)
		}
		sort.Slice(attrs, func(i, j int) bool { return attrs[i].SrcRange.Start.Byte < attrs[j].SrcRange.Start.Byte })
		for _, attr := range attrs {
			localsTarget.NestedTargets = append(localsTarget.NestedTargets, reference.Target{
				Addr:        append(localsAddr.Copy(), lang.AttrStep{Name: attr.Name}),
				ScopeId:     tfschema.TerragruntIncludeScope,
				RangePtr:    withFilename(attr.SrcRange, rel).Ptr(),
				DefRangePtr: withFilename(attr.NameRange, rel).Ptr(),
				Type:        cty.DynamicPseudoType,
				Name:        "local value",
				Description: lang.Markdown(fmt.Sprintf("Local value `%s` of `%s`: `%s = %s`", attr.Name, rel,
					attr.Name, strings.ReplaceAll(sourceText(attr.Expr, included.Bytes), "`", "'"))),
			})
		}
	}
	if localsTarget != nil {
		attrTypes := make(map[string]cty.Type, len(localsTarget.NestedTargets))
		for _, nested := range localsTarget.NestedTargets {
			attrTypes[nested.Addr[len(nested.Addr)-1].String()] = cty.DynamicPseudoType
		}
		localsTarget.Type = cty.Object(attrTypes)
		sort.Sort(localsTarget.NestedTargets)
		target.NestedTargets = reference.Targets{*localsTarget}
	}
	return target
}

func withFilename(rng hcl.Range, filename string) hcl.Range {
	rng.Filename = filename
	return rng
}
