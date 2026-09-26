// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/requiredproviders"
	"github.com/zclconf/go-cty/cty"
)

// lockFile is the dependency lock file of a root module.
const lockFile = ".terraform.lock.hcl"

// addRequiredProvider offers, on the type label of a resource, data
// source or ephemeral resource, to add its provider to required_providers
// when the module does not declare it there. The source is the one
// OpenTofu implies for an undeclared provider (hashicorp/<name>), and the
// version constraint allows the minor releases of the version the lock
// file pins.
func addRequiredProvider(env Env, doc Document, body *hclsyntax.Body, pos hcl.Pos) []Action {
	var block *hclsyntax.Block
	for _, b := range body.Blocks {
		switch b.Type {
		case "resource", "data", "ephemeral":
		default:
			continue
		}
		if len(b.LabelRanges) > 0 && within(b.LabelRanges[0], pos.Byte) {
			block = b
		}
	}
	if block == nil {
		return nil
	}
	local := providerLocalName(block)
	if local == "" || local == "terraform" || !hclsyntax.ValidIdentifier(local) {
		// terraform is the built-in provider
		return nil
	}

	sources, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	files := make(map[string]*hcl.File, len(sources))
	for _, f := range sources {
		files[filepath.Base(f.path)] = &hcl.File{Body: f.body, Bytes: f.src}
	}
	if _, ok := requiredproviders.Entries(files)[local]; ok {
		return nil
	}

	opts := requiredproviders.EntryOptions{CreateVersionsFile: env.CreateFiles}
	if v, ok := lockedVersion(env, doc.dir(), local); ok {
		opts.Version = v
	}
	e, ok := requiredproviders.AddEntryEdit(files, filepath.Base(doc.Path), local, "hashicorp/"+local, opts)
	if !ok {
		return nil
	}
	title := fmt.Sprintf("Add %q to required_providers", local)
	target := filepath.Join(doc.dir(), e.Filename)
	if e.Create {
		return []Action{{
			Title:  title + " in a new " + e.Filename,
			Create: []string{target},
			Edits:  []Edit{replace(target, nil, 0, 0, e.NewText)},
		}}
	}
	if e.NewBlock && target != doc.Path {
		title += " in " + e.Filename
	}
	src := files[e.Filename].Bytes
	return []Action{{Title: title, Edits: []Edit{replace(target, src, e.Range.Start.Byte, e.Range.End.Byte, e.NewText)}}}
}

// providerLocalName is the local name of the provider of a resource or
// data block: the provider meta-argument's, or the prefix of the type.
func providerLocalName(block *hclsyntax.Block) string {
	if attr, ok := block.Body.Attributes["provider"]; ok {
		expr := attr.Expr
		if idx, ok := expr.(*hclsyntax.IndexExpr); ok {
			expr = idx.Collection
		}
		if st, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
			return st.Traversal.RootName()
		}
		return ""
	}
	typ := block.Labels[0]
	if i := strings.IndexByte(typ, '_'); i > 0 {
		return typ[:i]
	}
	return typ
}

// lockedVersion returns "~> major.minor" for the version of the implied
// provider hashicorp/<name> which the module's lock file pins.
func lockedVersion(env Env, dir, name string) (string, bool) {
	src, ok := env.readFile(filepath.Join(dir, lockFile))
	if !ok {
		return "", false
	}
	body, ok := parse(lockFile, src)
	if !ok {
		return "", false
	}
	for _, block := range body.Blocks {
		if block.Type != "provider" || len(block.Labels) != 1 {
			continue
		}
		addr := block.Labels[0]
		if addr != "registry.opentofu.org/hashicorp/"+name && addr != "registry.terraform.io/hashicorp/"+name {
			continue
		}
		attr, ok := block.Body.Attributes["version"]
		if !ok {
			continue
		}
		v, diags := attr.Expr.Value(nil)
		if diags.HasErrors() || v.Type() != cty.String || v.IsNull() {
			continue
		}
		parts := strings.SplitN(v.AsString(), ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		return fmt.Sprintf("~> %d.%d", major, minor), true
	}
	return "", false
}
