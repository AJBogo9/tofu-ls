// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// refScope is what a reference may use where it appears.
type refScope struct {
	// each and count are available in blocks which set for_each or
	// count (in any of their declarations).
	each, count bool
	// self is available in provisioner, connection, precondition and
	// postcondition blocks.
	self bool
	// iterators are the dynamic block iterators in scope.
	iterators map[string]bool
}

func (s refScope) withIterator(name string) refScope {
	its := make(map[string]bool, len(s.iterators)+1)
	for k := range s.iterators {
		its[k] = true
	}
	its[name] = true
	s.iterators = its
	return s
}

// references reports references to resources, data sources, ephemeral
// resources and module calls (and module outputs, where the called module
// is indexed) which the module does not declare, and each, count and
// self used outside their scope. References to var and local are
// checked by the reference validation job.
//
// It needs every declaration, so it stays silent while any file of the
// module is broken. JSON files are not searched for references, but
// their declarations count.
func (c *checker) references(unresolved, unknownTypes bool) {
	if !c.complete {
		return
	}
	for _, name := range c.names {
		if !c.usable(name) || isJSON(name) {
			continue
		}
		body, ok := c.mod.Files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			c.topBlockRefs(block, unresolved, unknownTypes)
		}
	}
}

func (c *checker) topBlockRefs(block *hclsyntax.Block, unresolved, unknownTypes bool) {
	r := &refWalker{c: c, unresolved: unresolved, unknownTypes: unknownTypes}
	sc := refScope{}
	var skip map[string]bool

	switch block.Type {
	case "resource", "data", "ephemeral":
		if len(block.Labels) != 2 {
			return
		}
		addr := block.Labels[0] + "." + block.Labels[1]
		if block.Type != "resource" {
			addr = block.Type + "." + addr
		}
		sc.each, sc.count = c.idx.forEach[addr], c.idx.count[addr]
		skip = map[string]bool{"provider": true}
	case "module":
		if len(block.Labels) != 1 {
			return
		}
		addr := "module." + block.Labels[0]
		sc.each, sc.count = c.idx.forEach[addr], c.idx.count[addr]
		// source and version are checked as static values
		skip = map[string]bool{"providers": true, "source": true, "version": true}
	case "provider", "import":
		// OpenTofu allows for_each in provider and import blocks
		_, sc.each = block.Body.Attributes["for_each"]
		skip = map[string]bool{"to": true, "provider": true, "alias": true}
	case "output", "locals", "check":
	case "variable":
		// the type is not an expression, and the default is checked as
		// a static value
		skip = map[string]bool{"type": true, "default": true}
	case "removed":
		skip = map[string]bool{"from": true}
	default:
		// moved, terraform (backend, encryption, required_providers) and
		// unknown blocks
		return
	}

	r.body(block.Body, sc, skip)
}

type refWalker struct {
	c                        *checker
	unresolved, unknownTypes bool
}

func (r *refWalker) body(body *hclsyntax.Body, sc refScope, skip map[string]bool) {
	for name, attr := range body.Attributes {
		// provider and providers name provider configurations (such as
		// aws.west), wherever they appear
		if skip[name] || name == "provider" || name == "providers" {
			continue
		}
		r.expr(attr.Expr, sc)
	}
	for _, block := range body.Blocks {
		switch block.Type {
		case "dynamic":
			if len(block.Labels) != 1 {
				continue
			}
			iterator := block.Labels[0]
			if attr, ok := block.Body.Attributes["iterator"]; ok {
				if kw := hcl.ExprAsKeyword(attr.Expr); kw != "" {
					iterator = kw
				} else {
					continue
				}
			}
			if attr, ok := block.Body.Attributes["for_each"]; ok {
				r.expr(attr.Expr, sc)
			}
			inner := sc.withIterator(iterator)
			if attr, ok := block.Body.Attributes["labels"]; ok {
				r.expr(attr.Expr, inner)
			}
			for _, content := range block.Body.Blocks {
				if content.Type == "content" {
					r.body(content.Body, inner, nil)
				}
			}
		case "lifecycle":
			r.body(block.Body, sc, map[string]bool{"ignore_changes": true})
		case "provisioner", "connection", "precondition", "postcondition":
			inner := sc
			inner.self = true
			r.body(block.Body, inner, nil)
		default:
			r.body(block.Body, sc, nil)
		}
	}
}

func (r *refWalker) expr(expr hclsyntax.Expression, sc refScope) {
	for _, tr := range expr.Variables() {
		r.traversal(tr, sc)
	}
}

func (r *refWalker) traversal(tr hcl.Traversal, sc refScope) {
	root := tr.RootName()
	if sc.iterators[root] {
		return
	}
	c := r.c

	switch root {
	case "var", "local", "path", "terraform", "provider", "run":
		return
	case "each", "count", "self":
		if !r.unresolved {
			return
		}
		ok := map[string]bool{"each": sc.each, "count": sc.count, "self": sc.self}[root]
		if ok {
			return
		}
		detail := map[string]string{
			"each":  "The \"each\" object can be used only in \"module\" or \"resource\" blocks, and only when the \"for_each\" argument is set.",
			"count": "The \"count\" object can be used only in \"module\", \"resource\", and \"data\" blocks, and only when the \"count\" argument is set.",
			"self":  "The \"self\" object can be used only in provisioner and connection blocks, and in preconditions and postconditions.",
		}[root]
		summary := map[string]string{
			"each":  `Reference to "each" in context without for_each`,
			"count": `Reference to "count" in non-counted context`,
			"self":  `Invalid "self" reference`,
		}[root]
		c.add(hcl.DiagError, summary, detail, tr.SourceRange(), ilsp.CodedDiagnostic{
			Code: ilsp.CodeUnavailableScope,
			Data: map[string]interface{}{"address": addressOf(tr, 2)},
		})
		return
	case "data", "ephemeral":
		typ, name, ok := attrSteps(tr, 1)
		if !ok {
			return
		}
		declared := c.idx.data
		if root == "ephemeral" {
			declared = c.idx.ephemeral
		}
		if declared[typ+"."+name] {
			return
		}
		if root == "data" && r.unknownTypes && !c.idx.dataTypes[typ] {
			if c.unknownTypeRef(tr, "data", typ, hcl.RangeBetween(tr[0].SourceRange(), tr[1].SourceRange())) {
				return
			}
		}
		if r.unresolved {
			what := map[string]string{"data": "A data resource", "ephemeral": "An ephemeral resource"}[root]
			c.unresolvedRef(tr, root+"."+typ+"."+name, "Reference to undeclared resource",
				fmt.Sprintf("%s %q %q has not been declared in this module.", what, typ, name))
		}
		return
	case "module":
		name, ok := attrStep(tr, 1)
		if !ok {
			return
		}
		if !c.idx.modules[name] {
			if r.unresolved {
				c.unresolvedRef(tr, "module."+name, "Reference to undeclared module",
					fmt.Sprintf("No module call named %q is declared in this module.", name))
			}
			return
		}
		outputs, known := c.mod.ChildOutputs[name]
		if !known || !r.unresolved {
			return
		}
		// module.x.out, module.x[0].out or module.x["k"].out
		i := 2
		if i < len(tr) {
			if _, isIndex := tr[i].(hcl.TraverseIndex); isIndex {
				i++
			}
		}
		out, ok := attrStep(tr, i)
		if !ok || outputs[out] {
			return
		}
		c.add(hcl.DiagError, "Unsupported attribute",
			fmt.Sprintf("This object does not have an attribute named %q: the module called by module.%s declares no output %q.", out, name, out),
			hcl.RangeBetween(tr[0].SourceRange(), tr[i].SourceRange()),
			ilsp.CodedDiagnostic{
				Code: ilsp.CodeUnresolvedReference,
				Data: map[string]interface{}{"address": "module." + name + "." + out},
			})
		return
	}

	// a managed resource: type.name
	name, ok := attrStep(tr, 1)
	if !ok {
		return
	}
	if c.idx.resources[root+"."+name] {
		return
	}
	if r.unknownTypes && !c.idx.resTypes[root] {
		if c.unknownTypeRef(tr, "resource", root, tr[0].SourceRange()) {
			return
		}
	}
	if r.unresolved {
		c.unresolvedRef(tr, root+"."+name, "Reference to undeclared resource",
			fmt.Sprintf("A managed resource %q %q has not been declared in this module.", root, name))
	}
}

func (c *checker) unresolvedRef(tr hcl.Traversal, address, summary, detail string) {
	c.add(hcl.DiagError, summary, detail, tr.SourceRange(), ilsp.CodedDiagnostic{
		Code: ilsp.CodeUnresolvedReference,
		Data: map[string]interface{}{"address": address},
	})
}

// attrStep returns the name of step i when it is an attribute access.
func attrStep(tr hcl.Traversal, i int) (string, bool) {
	if i >= len(tr) {
		return "", false
	}
	a, ok := tr[i].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	return a.Name, true
}

// attrSteps returns the names of steps i and i+1.
func attrSteps(tr hcl.Traversal, i int) (string, string, bool) {
	a, ok := attrStep(tr, i)
	if !ok {
		return "", "", false
	}
	b, ok := attrStep(tr, i+1)
	return a, b, ok
}

// addressOf renders the first n steps of a traversal (root and
// attributes only).
func addressOf(tr hcl.Traversal, n int) string {
	s := tr.RootName()
	for i := 1; i < n && i < len(tr); i++ {
		a, ok := tr[i].(hcl.TraverseAttr)
		if !ok {
			break
		}
		s += "." + a.Name
	}
	return s
}
