// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfaddr "github.com/opentofu/registry-address"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/tofu/datadir"
	"github.com/zclconf/go-cty/cty"
)

// installation reports what tofu init has to do before the module can be
// validated or planned:
//
//   - a local module source which does not exist (an error, init cannot
//     help);
//   - a module call with a remote source which the module manifest does
//     not list;
//   - a required provider which the lock file does not list, or whose
//     locked version is not in .terraform/providers.
//
// The last two are reported only in a directory which has been
// initialized or has a lock file. A directory with neither is most often
// a module meant to be called from elsewhere (or a fresh clone), where a
// warning on every module and provider would be noise rather than news.
func (c *checker) installation() {
	calls := c.moduleSources()
	for _, call := range calls {
		if call.local {
			c.localModuleSource(call)
		}
	}

	if c.mod.FS == nil {
		return
	}
	dataDir := filepath.Join(c.mod.Path, datadir.DataDirName)
	fi, err := c.mod.FS.Stat(dataDir)
	hasDataDir := err == nil && fi.IsDir()
	_, lockErr := c.mod.FS.Stat(filepath.Join(c.mod.Path, ".terraform.lock.hcl"))
	hasLock := lockErr == nil
	if !hasDataDir && !hasLock {
		return
	}

	installed := c.installedModuleKeys()
	for _, call := range calls {
		if call.local || installed[call.name] {
			continue
		}
		c.add(hcl.DiagWarning, "Module not installed",
			fmt.Sprintf("Module %q (%s) is not installed. Run \"tofu init\" to install all modules required by this configuration.", call.name, call.source),
			call.rng, ilsp.CodedDiagnostic{
				Code: ilsp.CodeModuleNotInstalled,
				Data: map[string]interface{}{"module": call.name, "dir": c.mod.Path},
			})
	}

	c.providerInstallation(hasDataDir)
}

type moduleSource struct {
	name   string
	source string
	local  bool
	rng    hcl.Range
}

// moduleSources returns the module calls with a literal source, outside
// override files (which may change the source).
func (c *checker) moduleSources() []moduleSource {
	calls := make([]moduleSource, 0)
	for _, d := range c.idx.decls {
		if d.kind != "module" || d.override || d.shadowed || c.idx.overridden[d.address] || c.mod.Broken[d.file] {
			continue
		}
		content, _, _ := d.block.Body.PartialContent(metaArgsSchema)
		if content == nil {
			continue
		}
		attr, ok := content.Attributes["source"]
		if !ok {
			continue
		}
		if len(attr.Expr.Variables()) > 0 {
			continue
		}
		if syn, ok := attr.Expr.(hclsyntax.Expression); ok && hasFunctionCall(syn) {
			continue
		}
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() || val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
			continue
		}
		src := val.AsString()
		calls = append(calls, moduleSource{
			name:   d.name,
			source: src,
			local:  strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../"),
			rng:    attr.Expr.Range(),
		})
	}
	return calls
}

func (c *checker) localModuleSource(call moduleSource) {
	if c.mod.FS == nil {
		return
	}
	dir := filepath.Join(c.mod.Path, filepath.FromSlash(call.source))
	fi, err := c.mod.FS.Stat(dir)
	if err == nil && fi.IsDir() {
		return
	}
	detail := fmt.Sprintf("The module directory %q does not exist.", call.source)
	if err == nil {
		detail = fmt.Sprintf("The module source %q is not a directory.", call.source)
	}
	c.add(hcl.DiagError, "Unreadable module directory", detail, call.rng,
		ilsp.CodedDiagnostic{Code: ilsp.CodeModuleSourceMissing})
}

// installedModuleKeys returns the keys of .terraform/modules/modules.json,
// which for the module calls of the initialized directory are their names.
func (c *checker) installedModuleKeys() map[string]bool {
	keys := make(map[string]bool)
	b, err := c.mod.FS.ReadFile(filepath.Join(c.mod.Path, datadir.DataDirName, "modules", "modules.json"))
	if err != nil {
		return keys
	}
	var manifest struct {
		Modules []struct {
			Key string `json:"Key"`
		} `json:"Modules"`
	}
	if json.Unmarshal(b, &manifest) != nil {
		return keys
	}
	for _, m := range manifest.Modules {
		keys[m.Key] = true
	}
	return keys
}

// providerInstallation reports required providers which the lock file
// does not list, or whose locked version is not installed.
func (c *checker) providerInstallation(hasDataDir bool) {
	if !c.complete || c.mod.Meta == nil {
		return
	}
	lock := c.lockFile()
	ranges := c.providerRanges()

	addrs := make([]tfaddr.Provider, 0, len(c.mod.Meta.ProviderRequirements))
	for addr := range c.mod.Meta.ProviderRequirements {
		if norm, ok := normalizeProvider(addr); ok {
			addrs = append(addrs, norm)
		}
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].String() < addrs[j].String() })

	seen := make(map[tfaddr.Provider]bool)
	for _, addr := range addrs {
		if seen[addr] {
			continue
		}
		seen[addr] = true
		rng, ok := c.providerRange(addr, ranges)
		if !ok {
			continue
		}
		v, locked := lock[addr]
		var detail string
		switch {
		case !locked:
			detail = fmt.Sprintf("This configuration requires provider %s, but the lock file does not list it. Run \"tofu init\" to install it.", addr.ForDisplay())
		case !hasDataDir || !c.providerInstalled(addr, v.String()):
			detail = fmt.Sprintf("Provider %s %s is locked but not installed in .terraform/providers. Run \"tofu init\" to install it.", addr.ForDisplay(), v)
		default:
			continue
		}
		c.add(hcl.DiagWarning, "Provider not installed", detail, rng, ilsp.CodedDiagnostic{
			Code: ilsp.CodeProviderNotInstalled,
			Data: map[string]interface{}{"provider": addr.String(), "dir": c.mod.Path},
		})
	}
}

func (c *checker) providerInstalled(addr tfaddr.Provider, v string) bool {
	dir := filepath.Join(c.mod.Path, datadir.DataDirName, "providers", addr.Hostname.String(), addr.Namespace, addr.Type, v)
	fi, err := c.mod.FS.Stat(dir)
	return err == nil && fi.IsDir()
}

// providerRanges returns, by local name, the range of each provider's
// source in required_providers (or its whole entry when it has none).
func (c *checker) providerRanges() map[string]hcl.Range {
	ranges := make(map[string]hcl.Range)
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type != "terraform" {
				continue
			}
			for _, inner := range block.Body.Blocks {
				if inner.Type != "required_providers" {
					continue
				}
				for local, attr := range inner.Body.Attributes {
					if _, ok := ranges[local]; ok {
						continue
					}
					rng := attr.Expr.Range()
					if obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr); ok {
						for _, item := range obj.Items {
							if hcl.ExprAsKeyword(item.KeyExpr) == "source" {
								rng = item.ValueExpr.Range()
							}
						}
					}
					ranges[local] = rng
				}
			}
		}
	}
	return ranges
}

// providerRange finds where to report a provider: its required_providers
// entry, else the type label of the first resource or data source which
// implies it.
func (c *checker) providerRange(addr tfaddr.Provider, ranges map[string]hcl.Range) (hcl.Range, bool) {
	locals := make([]string, 0)
	for ref, a := range c.mod.Meta.ProviderReferences {
		if ref.Alias != "" {
			continue
		}
		if norm, ok := normalizeProvider(a); ok && norm.Equals(addr) {
			locals = append(locals, ref.LocalName)
		}
	}
	sort.Strings(locals)
	for _, local := range locals {
		if rng, ok := ranges[local]; ok {
			return rng, true
		}
	}
	for _, d := range c.idx.decls {
		if d.kind != "resource" && d.kind != "data" && d.kind != "ephemeral" {
			continue
		}
		if d.shadowed || c.mod.Broken[d.file] || len(d.block.LabelRanges) == 0 {
			continue
		}
		for _, local := range locals {
			if impliedProviderName(d.typ) == local {
				return d.block.LabelRanges[0], true
			}
		}
	}
	return hcl.Range{}, false
}
