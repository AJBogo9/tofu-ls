// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// deletion is a declaration Safe delete removes.
type deletion struct {
	// kind is variable, local, output or data source.
	kind string
	// name is the name, or type.name for a data source.
	name string
	// ref is how the module refers to it, such as var.x.
	ref []string
}

func (d deletion) refText() string {
	text := d.ref[0]
	for _, s := range d.ref[1:] {
		text += "." + s
	}
	return text
}

// deletionAt returns the declaration whose header (or, for a local value,
// name) holds the cursor.
func deletionAt(r *request) (deletion, bool) {
	b := r.rng.Start.Byte
	var header func(block *hclsyntax.Block) (deletion, bool)
	header = func(block *hclsyntax.Block) (deletion, bool) {
		if !within(block.Range(), b) {
			return deletion{}, false
		}
		switch block.Type {
		case "variable", "output":
			if len(block.Labels) == 1 && blockHeaderHolds(block, b) {
				name := block.Labels[0]
				ref := []string{"var", name}
				if block.Type == "output" {
					ref = []string{"output", name}
				}
				return deletion{kind: block.Type, name: name, ref: ref}, true
			}
		case "data":
			if len(block.Labels) == 2 && blockHeaderHolds(block, b) {
				return deletion{kind: "data source", name: block.Labels[0] + "." + block.Labels[1],
					ref: []string{"data", block.Labels[0], block.Labels[1]}}, true
			}
		case "check":
			for _, nested := range block.Body.Blocks {
				if nested.Type == "data" {
					if d, ok := header(nested); ok {
						return d, true
					}
				}
			}
		case "locals":
			for name, attr := range block.Body.Attributes {
				if within(attr.NameRange, b) {
					return deletion{kind: "local", name: name, ref: []string{"local", name}}, true
				}
			}
		}
		return deletion{}, false
	}
	for _, block := range r.body.Blocks {
		if d, ok := header(block); ok {
			return d, true
		}
	}
	return deletion{}, false
}

func (d deletion) title() string {
	switch d.kind {
	case "data source":
		return "Safe delete data source data." + d.name
	}
	return fmt.Sprintf("Safe delete %s %q", d.kind, d.name)
}

func offerSafeDelete(r *request) []Action {
	d, ok := deletionAt(r)
	if !ok {
		return nil
	}
	return []Action{r.offer(refSafeDelete, KindRefactor, d.title(), safeDeleteRefusal(r, d))}
}

// safeDeleteRefusal lists the uses of the declaration, which keep it
// from being deleted.
func safeDeleteRefusal(r *request, d deletion) error {
	dir := r.doc.dir()
	if json := r.env.jsonConfig(dir); json != "" {
		return refusef("%s cannot be searched for uses", json)
	}
	files, ok := moduleSources(r.env, r.doc)
	if !ok {
		return Refusal("a file of the module has syntax errors")
	}
	tests, ok := r.env.parsedFiles(r.env.testFiles(dir))
	if !ok {
		return Refusal("a test file of the module has syntax errors")
	}
	var uses []use
	switch d.kind {
	case "output":
		// the module's own files cannot refer to its outputs; its callers
		// and its tests can
		callers, ok := r.callers()
		if !ok {
			return Refusal("the modules which call this module could not be read")
		}
		for _, c := range callers {
			callerFiles, ok := r.env.parsedFiles(r.env.moduleFiles(c.Dir))
			if !ok {
				return refusef("a file of the calling module in %s has syntax errors", relTo(dir, c.Dir))
			}
			for _, u := range usesIn(callerFiles, refersTo("module", c.Name)) {
				if readsOutput(u, d.name) {
					uses = append(uses, u)
				}
			}
		}
		uses = append(uses, usesIn(tests, refersTo("output", d.name))...)
		for _, u := range usesIn(tests, func(tr hcl.Traversal) bool { return tr.RootName() == "run" }) {
			if len(u.expr.Traversal) > 2 {
				if attr, ok := u.expr.Traversal[2].(hcl.TraverseAttr); ok && attr.Name == d.name {
					uses = append(uses, u)
				}
			}
		}
	default:
		for _, u := range usesIn(append(files, tests...), refersTo(d.ref[0], d.ref[1:]...)) {
			if d.kind == "variable" && u.chain != nil && u.chain[0].Type == "variable" && len(u.chain[0].Labels) == 1 && u.chain[0].Labels[0] == d.name {
				// its own validation rules
				continue
			}
			if d.kind == "local" && len(u.chain) == 1 && u.chain[0].Type == "locals" && u.attr.Name == d.name {
				continue
			}
			uses = append(uses, u)
		}
	}
	if len(uses) > 0 {
		return refusef("%s is used %s: %s", d.refText(), times(len(uses)), listUses(dir, uses))
	}
	return nil
}

// readsOutput tells whether a reference to a module call reads the
// output: module.x.out, module.x[k].out, module.x[*].out, or the whole
// module.x object.
func readsOutput(u use, output string) bool {
	tr := u.expr.Traversal
	rest := tr[2:]
	if len(rest) > 0 {
		if _, ok := rest[0].(hcl.TraverseIndex); ok {
			rest = rest[1:]
		}
	}
	if len(rest) > 0 {
		attr, ok := rest[0].(hcl.TraverseAttr)
		return ok && attr.Name == output
	}
	if splat, ok := u.parent.(*hclsyntax.SplatExpr); ok && splat.Source == u.expr {
		if each, ok := splat.Each.(*hclsyntax.RelativeTraversalExpr); ok && len(each.Traversal) > 0 {
			attr, ok := each.Traversal[0].(hcl.TraverseAttr)
			return ok && attr.Name == output
		}
	}
	// the whole object reads every output
	return true
}

func resolveSafeDelete(r *request) (Action, error) {
	d, ok := deletionAt(r)
	if !ok {
		return Action{}, Refusal("there is no declaration here")
	}
	if err := safeDeleteRefusal(r, d); err != nil {
		return Action{}, err
	}
	var actions []Action
	switch d.kind {
	case "variable":
		actions = removeUnusedVariable(r.env, r.doc, Diagnostic{Data: map[string]interface{}{"name": d.name}})
	case "local":
		actions = removeUnusedLocal(r.env, r.doc, Diagnostic{Data: map[string]interface{}{"name": d.name}})
	case "data source":
		actions = removeUnusedDataSource(r.env, r.doc, Diagnostic{Data: map[string]interface{}{"address": "data." + d.name}})
	case "output":
		actions = removeOutput(r.env, r.doc, d.name)
	}
	if len(actions) != 1 {
		return Action{}, refusef("%s could not be removed", d.refText())
	}
	a := actions[0]
	a.Title, a.Kind, a.Preferred = d.title(), KindRefactor, false
	return a, nil
}

// removeOutput deletes the output's declarations.
func removeOutput(env Env, doc Document, name string) []Action {
	files, ok := moduleSources(env, doc)
	if !ok {
		return nil
	}
	dels := newDeletions()
	for _, f := range files {
		for _, block := range f.body.Blocks {
			if block.Type == "output" && len(block.Labels) == 1 && block.Labels[0] == name {
				dels.add(f.path, f.src, block.Range().Start.Byte, block.Range().End.Byte)
			}
		}
	}
	if !dels.has(doc.Path) {
		return nil
	}
	return []Action{{Edits: dels.edits()}}
}
