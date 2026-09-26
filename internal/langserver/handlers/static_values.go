// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
	tfmod "github.com/opentofu/opentofu-schema/module"
	"github.com/opentofu/tofu-ls/internal/document"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/staticval"
)

// staticEvaluator loads the module of dir for static evaluation. When
// other indexed modules call dir through a local source, dir is a child
// module: its variables get their values from those module calls, not
// from tfvars files. Evaluation stops once ctx is done.
func (svc *service) staticEvaluator(ctx context.Context, dir string, bodySchema *schema.BodySchema) (*staticval.Evaluator, error) {
	mod, err := staticval.LoadModule(svc.fs, dir)
	if err != nil {
		return nil, err
	}
	staticval.AddCallers(svc.fs, mod, maxCallerNesting, svc.indexedCallers)
	ev := staticval.NewEvaluatorContext(ctx, mod)
	ev.SetEnv(svc.staticEnv(dir, bodySchema))
	return ev, nil
}

// maxCallerNesting is how many levels of calling modules are resolved, so
// that a caller that is itself a child module gets its values from its
// own callers instead of its defaults.
const maxCallerNesting = 3

// indexedCallers returns the indexed modules that call dir through a
// local source.
func (svc *service) indexedCallers(dir string) []staticval.Caller {
	if svc.features == nil || svc.features.Modules == nil {
		return nil
	}
	records, err := svc.features.Modules.Store.List()
	if err != nil {
		return nil
	}
	var callers []staticval.Caller
	for _, rec := range records {
		var parent *staticval.Module
		for name, mc := range rec.Meta.ModuleCalls {
			src := mc.RawSourceAddr
			if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
				continue
			}
			if filepath.Clean(filepath.Join(rec.Path(), src)) != filepath.Clean(dir) {
				continue
			}
			if parent == nil {
				parent, err = staticval.LoadModule(svc.fs, rec.Path())
				if err != nil {
					break
				}
			}
			callers = append(callers, staticval.Caller{Parent: parent, Name: name})
		}
	}
	return callers
}

// staticEnv connects the evaluator to the provider schemas and installed
// modules of the module in dir. A non-nil bodySchema (the one the
// document's decoder already holds) saves merging the schema again.
func (svc *service) staticEnv(dir string, bodySchema *schema.BodySchema) staticval.Env {
	env := staticval.Env{
		LoadModule: func(childDir string) (*staticval.Module, error) {
			return staticval.LoadModule(svc.fs, childDir)
		},
	}
	if svc.features == nil {
		return env
	}
	if svc.features.RootModules != nil {
		env.ModuleDir = func(source string) (string, bool) {
			// The manifest is keyed by the normalized source address and
			// holds directories relative to the root module.
			addr := tfmod.ParseModuleSourceAddr(source)
			if addr == nil {
				return "", false
			}
			installed, ok := svc.features.RootModules.InstalledModulePath(dir, addr.String())
			if !ok {
				return "", false
			}
			if !filepath.IsAbs(installed) {
				installed = filepath.Join(dir, installed)
			}
			return installed, true
		}
	}
	if svc.features.Modules != nil {
		loaded := bodySchema != nil
		env.Attribute = func(blockType, typeName, attr string) (*staticval.AttributeInfo, bool) {
			if !loaded {
				loaded = true
				pathCtx, err := svc.features.Modules.PathContext(lang.Path{
					Path:       dir,
					LanguageID: ilsp.OpenTofu.String(),
				})
				if err == nil {
					bodySchema = pathCtx.Schema
				}
			}
			return attributeInfo(bodySchema, blockType, typeName, attr)
		}
	}
	return env
}

// attributeInfo looks up an attribute of a resource or data source type in
// the module schema.
func attributeInfo(bodySchema *schema.BodySchema, blockType, typeName, attr string) (*staticval.AttributeInfo, bool) {
	if bodySchema == nil {
		return nil, false
	}
	bs, ok := bodySchema.Blocks[blockType]
	if !ok {
		return nil, false
	}
	// The body keyed by the type label alone is the common case.
	body := bs.DependentBody[schema.NewSchemaKey(schema.DependencyKeys{
		Labels: []schema.LabelDependent{{Index: 0, Value: typeName}},
	})]
	labelJSON, _ := json.Marshal(typeName)
	for key, dep := range bs.DependentBody {
		if body != nil {
			break
		}
		// Cheap filter before decoding: providers have thousands of keys.
		if !strings.Contains(string(key), string(labelJSON)) {
			continue
		}
		var keys schema.DependencyKeys
		if err := json.Unmarshal([]byte(key), &keys); err != nil {
			continue
		}
		if len(keys.Labels) == 0 || keys.Labels[0].Value != typeName {
			continue
		}
		// Prefer the body that does not depend on a provider alias.
		if body == nil || len(keys.Attributes) == 0 {
			body = dep
		}
	}
	if body == nil {
		return nil, false
	}
	aSchema, ok := body.Attributes[attr]
	if !ok {
		return nil, false
	}
	info := &staticval.AttributeInfo{
		Description: aSchema.Description.Value,
		Required:    aSchema.IsRequired,
		Optional:    aSchema.IsOptional,
		Computed:    aSchema.IsComputed,
		Sensitive:   aSchema.IsSensitive,
		Deprecated:  aSchema.IsDeprecated,
	}
	if aSchema.Constraint != nil {
		info.Type = aSchema.Constraint.FriendlyName()
	}
	if body.DocsLink != nil {
		info.DocsURL = body.DocsLink.URL
	}
	return info, true
}

// staticValueHover answers hovers on variables, locals, iteration symbols,
// module calls and provider attributes with values and their sources.
func (svc *service) staticValueHover(ctx context.Context, doc *document.Document, pos hcl.Pos, bodySchema *schema.BodySchema) (*lang.HoverData, bool) {
	langID := ilsp.ParseLanguageID(doc.LanguageID)
	if langID != ilsp.OpenTofu && langID != ilsp.OpenTofuVars {
		return nil, false
	}
	dir := doc.Dir.Path()
	var h *staticval.Hover
	var ok bool
	if langID == ilsp.OpenTofuVars {
		// The module's schema, not the tfvars one, describes resources.
		ev, err := svc.staticEvaluator(ctx, dir, nil)
		if err != nil {
			return nil, false
		}
		var f *hcl.File
		if strings.HasSuffix(doc.Filename, ".json") {
			f, _ = hcljson.Parse(doc.Text, doc.Filename)
		} else {
			f, _ = hclsyntax.ParseConfig(doc.Text, doc.Filename, hcl.InitialPos)
		}
		h, ok = ev.VarsFileHover(doc.Filename, f, pos)
	} else {
		f, _ := hclsyntax.ParseConfig(doc.Text, doc.Filename, hcl.InitialPos)
		if !staticval.WantsHover(f, pos) {
			return nil, false
		}
		ev, err := svc.staticEvaluator(ctx, dir, bodySchema)
		if err != nil {
			return nil, false
		}
		h, ok = ev.HoverAt(doc.Filename, pos, svc.staticEnv(dir, bodySchema))
	}
	if !ok || ctx.Err() != nil {
		return nil, false
	}
	return &lang.HoverData{
		Content: lang.Markdown(h.Content),
		Range:   h.Range,
	}, true
}
