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

	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	var tf, rp *hclsyntax.Block
	var tfFile sourceFile
	for _, f := range files {
		if strings.HasSuffix(f.path, ".json") {
			continue
		}
		for _, b := range f.body.Blocks {
			if b.Type != "terraform" {
				continue
			}
			for _, nested := range b.Body.Blocks {
				if nested.Type != "required_providers" {
					continue
				}
				if _, ok := nested.Body.Attributes[local]; ok {
					return nil
				}
				if rp == nil {
					tf, rp, tfFile = b, nested, f
				}
			}
			if tf == nil || (rp == nil && filepath.Base(f.path) == "versions.tf" && filepath.Base(tfFile.path) != "versions.tf") {
				tf, tfFile = b, f
			}
		}
	}

	entry := fmt.Sprintf("%s = {\n  source = %q\n", local, "hashicorp/"+local)
	if v, ok := lockedVersion(env, doc.dir(), local); ok {
		entry += fmt.Sprintf("  version = %q\n", v)
	}
	entry += "}"
	title := fmt.Sprintf("Add %q to required_providers", local)

	switch {
	case rp != nil:
		edits := formattedBlock(tfFile.path, tfFile.src, tf, rp, insertInBody(tfFile.path, tfFile.src, rp, []string{entry}, nil))
		return []Action{{Title: title, Edits: edits}}
	case tf != nil:
		nested := "required_providers {\n" + indentLines(entry, indentUnit) + "\n}"
		edits := formattedBlock(tfFile.path, tfFile.src, tf, tf, insertInBody(tfFile.path, tfFile.src, tf, nil, []string{nested}))
		return []Action{{Title: title, Edits: edits}}
	}

	text := "terraform {\n  required_providers {\n" + indentLines(entry, "    ") + "\n  }\n}\n"
	target := filepath.Join(doc.dir(), "versions.tf")
	if src, ok := env.readFile(target); ok {
		return []Action{{Title: title + " in versions.tf", Edits: []Edit{appendBlock(target, src, text)}}}
	}
	if env.CreateFiles {
		return []Action{{
			Title:  title + " in a new versions.tf",
			Create: []string{target},
			Edits:  []Edit{replace(target, nil, 0, 0, text)},
		}}
	}
	return []Action{{Title: title, Edits: []Edit{appendBlock(doc.Path, doc.Text, text)}}}
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
