// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// maxBlockDepth limits how deep required nested blocks are filled in.
const maxBlockDepth = 3

// addRequiredArguments inserts every required argument the block lacks,
// with an empty value of its type as a placeholder, and an empty nested
// block (holding its own required arguments) for every required block.
func addRequiredArguments(env Env, doc Document, diag Diagnostic) []Action {
	body, ok := env.parse(doc.Path, doc.Text)
	if !ok {
		return nil
	}
	chain := blockChain(body, diag.Range)
	if len(chain) == 0 {
		return nil
	}
	block := chain[len(chain)-1]

	var attrs, blocks, names []string
	if bs := bodySchemaFor(env, doc.dir(), chain); bs != nil {
		attrs, blocks, names = requiredItems(block.Body, bs, 0)
	} else {
		// without a schema the types are not known
		for _, name := range diag.dataStrings("attributes") {
			if _, ok := block.Body.Attributes[name]; !ok && hclsyntax.ValidIdentifier(name) {
				attrs = append(attrs, name+" = null")
				names = append(names, name)
			}
		}
		for _, name := range diag.dataStrings("blocks") {
			if hclsyntax.ValidIdentifier(name) {
				blocks = append(blocks, name+" {\n}")
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}

	var title string
	switch {
	case len(names) == 1 && len(attrs) == 1:
		title = fmt.Sprintf("Add required argument %q", names[0])
	case len(names) == 1:
		title = fmt.Sprintf("Add required block %q", names[0])
	case len(names) <= 4:
		title = "Add required arguments: " + strings.Join(names, ", ")
	default:
		title = fmt.Sprintf("Add required arguments: %s and %d more", strings.Join(names[:3], ", "), len(names)-3)
	}
	edits := formattedBlock(doc.Path, doc.Text, chain[0], block, insertInBody(doc.Path, doc.Text, block, attrs, blocks))
	return []Action{{Title: title, Preferred: true, Edits: edits}}
}

// requiredItems returns the text of the required arguments and blocks
// that body lacks, and their names, sorted by name. Arguments that only
// take a reference (such as the from of a moved block) and labeled blocks
// need what only the user can give, so they are left out.
func requiredItems(body *hclsyntax.Body, bs *schema.BodySchema, depth int) (attrs, blocks, names []string) {
	attrNames := make([]string, 0)
	for name, a := range bs.Attributes {
		if a == nil || !a.IsRequired || isReference(a.Constraint) {
			continue
		}
		if body != nil {
			if _, ok := body.Attributes[name]; ok {
				continue
			}
		}
		attrNames = append(attrNames, name)
	}
	sort.Strings(attrNames)
	for _, name := range attrNames {
		attrs = append(attrs, fmt.Sprintf("%s = %s", name, placeholder(bs.Attributes[name].Constraint)))
	}

	blockNames := make([]string, 0)
	for name, b := range bs.Blocks {
		if b == nil || b.MinItems == 0 || len(b.Labels) > 0 || name == "dynamic" {
			continue
		}
		present := 0
		if body != nil {
			for _, nested := range body.Blocks {
				if nested.Type == name {
					present++
				}
			}
		}
		if present < int(b.MinItems) {
			blockNames = append(blockNames, name)
		}
	}
	sort.Strings(blockNames)
	for _, name := range blockNames {
		b := bs.Blocks[name]
		var inner []string
		if depth < maxBlockDepth && b.Body != nil {
			a, nb, _ := requiredItems(nil, b.Body, depth+1)
			inner = append(a, nb...)
		}
		text := name + " {\n"
		if len(inner) > 0 {
			text += indentLines(strings.Join(inner, "\n"), indentUnit) + "\n"
		}
		blocks = append(blocks, text+"}")
	}
	return attrs, blocks, append(attrNames, blockNames...)
}
