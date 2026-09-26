// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// decl is one declaration of a module: a block, or one local value.
type decl struct {
	// kind is the block type, or "local".
	kind string
	// address is how OpenTofu names the declaration: var.x, output.x,
	// local.x, module.x, aws_s3_bucket.x, data.aws_ami.x, ephemeral.t.x,
	// provider.aws or provider.aws.west.
	address string
	// name is the declared name (the second label of resources).
	name string
	// typ is the resource type of resources, data sources and ephemeral
	// resources.
	typ string

	file  string
	rng   hcl.Range
	block *hcl.Block

	override    bool
	shadowed    bool
	checkScoped bool
	// aliasUnknown is true for a provider block whose alias is not a
	// static string.
	aliasUnknown bool
}

// index holds every declaration of the module. The sets include the
// declarations of override files and of shadowed files, so that a
// reference is flagged only when nothing could declare its target.
type index struct {
	decls []*decl

	resources map[string]bool // type.name
	data      map[string]bool // type.name, check-scoped ones included
	ephemeral map[string]bool // type.name
	modules   map[string]bool
	dataTypes map[string]bool
	resTypes  map[string]bool

	// forEach and count record, by address, whether any declaration
	// (primary or override) sets the meta-argument.
	forEach map[string]bool
	count   map[string]bool

	// overridden records the addresses which an override file declares.
	overridden map[string]bool
}

var topSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "output", LabelNames: []string{"name"}},
		{Type: "locals"},
		{Type: "module", LabelNames: []string{"name"}},
		{Type: "resource", LabelNames: []string{"type", "name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "ephemeral", LabelNames: []string{"type", "name"}},
		{Type: "provider", LabelNames: []string{"name"}},
		{Type: "check", LabelNames: []string{"name"}},
	},
}

var checkSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "data", LabelNames: []string{"type", "name"}},
	},
}

var metaArgsSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "for_each"},
		{Name: "count"},
		{Name: "provider"},
		{Name: "alias"},
		{Name: "source"},
	},
}

func buildIndex(c *checker) *index {
	idx := &index{
		resources:  make(map[string]bool),
		data:       make(map[string]bool),
		ephemeral:  make(map[string]bool),
		modules:    make(map[string]bool),
		dataTypes:  make(map[string]bool),
		resTypes:   make(map[string]bool),
		forEach:    make(map[string]bool),
		count:      make(map[string]bool),
		overridden: make(map[string]bool),
	}

	for _, name := range c.names {
		f := c.mod.Files[name]
		if f == nil || f.Body == nil {
			continue
		}
		override := isOverrideFile(name)
		shadowed := c.shadowed(name)
		content, _, _ := f.Body.PartialContent(topSchema)
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			for _, d := range blockDecls(name, block) {
				d.override = override
				d.shadowed = shadowed
				idx.add(d)
			}
			if block.Type == "check" {
				checkContent, _, _ := block.Body.PartialContent(checkSchema)
				if checkContent == nil {
					continue
				}
				for _, inner := range checkContent.Blocks {
					for _, d := range blockDecls(name, inner) {
						d.override = override
						d.shadowed = shadowed
						d.checkScoped = true
						idx.add(d)
					}
				}
			}
		}
	}

	return idx
}

func (idx *index) add(d *decl) {
	idx.decls = append(idx.decls, d)
	switch d.kind {
	case "resource":
		idx.resources[d.typ+"."+d.name] = true
		idx.resTypes[d.typ] = true
	case "data":
		idx.data[d.typ+"."+d.name] = true
		idx.dataTypes[d.typ] = true
	case "ephemeral":
		idx.ephemeral[d.typ+"."+d.name] = true
	case "module":
		idx.modules[d.name] = true
	}
	if d.override {
		idx.overridden[d.address] = true
	}
	if d.block != nil {
		meta, _, _ := d.block.Body.PartialContent(metaArgsSchema)
		if meta != nil {
			if _, ok := meta.Attributes["for_each"]; ok {
				idx.forEach[d.address] = true
			}
			if _, ok := meta.Attributes["count"]; ok {
				idx.count[d.address] = true
			}
		}
	}
}

// blockDecls returns the declarations a top-level block makes.
func blockDecls(file string, block *hcl.Block) []*decl {
	d := &decl{kind: block.Type, file: file, block: block, rng: labelsRange(block)}
	switch block.Type {
	case "variable":
		if len(block.Labels) != 1 {
			return nil
		}
		d.name = block.Labels[0]
		d.address = "var." + d.name
	case "output":
		if len(block.Labels) != 1 {
			return nil
		}
		d.name = block.Labels[0]
		d.address = "output." + d.name
	case "module":
		if len(block.Labels) != 1 {
			return nil
		}
		d.name = block.Labels[0]
		d.address = "module." + d.name
	case "resource", "data", "ephemeral":
		if len(block.Labels) != 2 {
			return nil
		}
		d.typ, d.name = block.Labels[0], block.Labels[1]
		switch block.Type {
		case "resource":
			d.address = d.typ + "." + d.name
		default:
			d.address = block.Type + "." + d.typ + "." + d.name
		}
	case "provider":
		if len(block.Labels) != 1 {
			return nil
		}
		d.name = block.Labels[0]
		d.address = "provider." + d.name
		meta, _, _ := block.Body.PartialContent(metaArgsSchema)
		if meta != nil {
			if attr, ok := meta.Attributes["alias"]; ok {
				val, diags := attr.Expr.Value(nil)
				if diags.HasErrors() || val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
					d.aliasUnknown = true
				} else {
					d.address += "." + val.AsString()
				}
			}
		}
	case "locals":
		attrs, _ := block.Body.JustAttributes()
		decls := make([]*decl, 0, len(attrs))
		for _, attr := range attrs {
			decls = append(decls, &decl{
				kind:    "local",
				name:    attr.Name,
				address: "local." + attr.Name,
				file:    file,
				rng:     attr.NameRange,
			})
		}
		// JustAttributes returns a map; keep source order
		sortDecls(decls)
		return decls
	default:
		return nil
	}
	return []*decl{d}
}

func sortDecls(decls []*decl) {
	for i := 1; i < len(decls); i++ {
		for j := i; j > 0 && decls[j].rng.Start.Byte < decls[j-1].rng.Start.Byte; j-- {
			decls[j], decls[j-1] = decls[j-1], decls[j]
		}
	}
}
