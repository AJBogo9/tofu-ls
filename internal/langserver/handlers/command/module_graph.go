// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/langserver/cmd"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	"github.com/opentofu/tofu-ls/internal/uri"
	"github.com/zclconf/go-cty/cty"
)

const moduleGraphVersion = 0

// moduleGraphResponse is the dependency graph of one module directory:
// its declarations as nodes, and an edge from every declaration to each
// declaration that references it.
//
// Edges come from the traversals in the parsed configuration
// (hclsyntax.Variables), the same source OpenTofu itself builds its
// dependency graph from. They therefore do not depend on provider
// schemas being available, and they include references in nested
// blocks, for expressions and templates.
type moduleGraphResponse struct {
	FormatVersion int               `json:"v"`
	ModuleURI     string            `json:"module_uri"`
	Nodes         []moduleGraphNode `json:"nodes"`
	Edges         []moduleGraphEdge `json:"edges"`
	// ParseErrors are the syntax errors of the module's files. The parser
	// drops what follows an error, so with any of them the graph may be
	// missing declarations and references.
	ParseErrors []moduleGraphParseError `json:"parse_errors,omitempty"`
}

type moduleGraphParseError struct {
	URI     string    `json:"uri"`
	Range   lsp.Range `json:"range"`
	Message string    `json:"message"`
}

type moduleGraphNode struct {
	// ID is the address the node is referenced by, e.g. var.region,
	// local.name, aws_instance.web, data.aws_ami.ubuntu, module.vpc,
	// output.ip, or provider.aws.west for a provider configuration.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Type is the resource or data source type, or the provider
	// local name.
	Type string `json:"type,omitempty"`
	URI  string `json:"uri"`
	// Range covers the whole block (or the attribute, for a local).
	Range lsp.Range `json:"range"`
	// NameRange covers the block header (or the local's name).
	NameRange lsp.Range `json:"name_range"`
	// Description is the literal description of a variable or an output.
	Description string `json:"description,omitempty"`
	// Detail is source text that says what the node is: the type
	// constraint of a variable, the expression of a local, the source
	// of a module call. It is code as written, never an evaluated value.
	Detail    string `json:"detail,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	// Repeat is "count" or "for_each" when the block is repeated.
	Repeat string            `json:"repeat,omitempty"`
	Child  *moduleGraphChild `json:"child,omitempty"`
}

// moduleGraphChild describes the boundary of a module call whose source
// directory is known and parsed.
type moduleGraphChild struct {
	URI string `json:"uri"`
	// Inputs are the arguments of the call, each with the child's
	// variable declaration when it exists.
	Inputs []moduleGraphPort `json:"inputs"`
	// Outputs are the child's outputs this module references
	// (module.<name>.<output>).
	Outputs []moduleGraphPort `json:"outputs"`
}

type moduleGraphPort struct {
	Name  string     `json:"name"`
	URI   string     `json:"uri,omitempty"`
	Range *lsp.Range `json:"range,omitempty"`
}

// moduleGraphEdge says that To references From, so values flow
// from From into To.
type moduleGraphEdge struct {
	From string           `json:"from"`
	To   string           `json:"to"`
	Refs []moduleGraphRef `json:"refs"`
}

type moduleGraphRef struct {
	URI   string    `json:"uri"`
	Range lsp.Range `json:"range"`
	// Attribute is the attribute path inside To that holds the
	// reference, e.g. triggers or lifecycle.replace_triggered_by.
	// It is empty for a local, whose whole expression is the value.
	Attribute string `json:"attribute,omitempty"`
	// Text is the reference as written, e.g. module.app.url.
	Text string `json:"text"`
}

// moduleGraphChildFiles resolves the directory and parsed files of the
// module called as name with the given source. It returns an empty
// directory when the child is not known.
type moduleGraphChildFiles func(name, source string) (dir string, files map[string]*hcl.File)

func (h *CmdHandler) ModuleGraphHandler(ctx context.Context, args cmd.CommandArgs) (interface{}, error) {
	response := moduleGraphResponse{
		FormatVersion: moduleGraphVersion,
		Nodes:         make([]moduleGraphNode, 0),
		Edges:         make([]moduleGraphEdge, 0),
	}

	modUri, ok := args.GetString("uri")
	if !ok || modUri == "" {
		return response, fmt.Errorf("%w: expected module uri argument to be set", jrpc2.InvalidParams.Err())
	}

	if !uri.IsURIValid(modUri) {
		return response, fmt.Errorf("URI %q is not valid", modUri)
	}

	modPath, err := uri.PathFromURI(modUri)
	if err != nil {
		return response, err
	}

	mod, err := h.ModulesFeature.Store.ModuleRecordByPath(modPath)
	if err != nil {
		return response, err
	}

	childFiles := func(name, source string) (string, map[string]*hcl.File) {
		dir := ""
		if isLocalModuleSource(source) {
			dir = filepath.Join(modPath, filepath.FromSlash(source))
		} else if h.RootModulesFeature != nil {
			installed, err := h.RootModulesFeature.InstalledModuleCalls(modPath)
			if err == nil {
				if mc, ok := installed[name]; ok && mc.Path != "" {
					dir = mc.Path
					if !filepath.IsAbs(dir) {
						dir = filepath.Join(modPath, dir)
					}
				}
			}
		}
		if dir == "" {
			return "", nil
		}
		child, err := h.ModulesFeature.Store.ModuleRecordByPath(dir)
		if err != nil || child.ParsedModuleFiles == nil {
			return "", nil
		}
		return dir, child.ParsedModuleFiles.AsMap()
	}

	response = buildModuleGraph(modPath, mod.ParsedModuleFiles.AsMap(), childFiles)
	response.ParseErrors = moduleGraphParseErrors(modPath, mod.ModuleDiagnostics[globalAst.HCLParsingSource])
	return response, nil
}

// moduleGraphParseErrors lists the error diagnostics of parsing, sorted
// by file and position.
func moduleGraphParseErrors(modPath string, diags ast.ModDiags) []moduleGraphParseError {
	errs := make([]moduleGraphParseError, 0)
	for filename, fileDiags := range diags {
		for _, diag := range fileDiags {
			if diag.Severity != hcl.DiagError {
				continue
			}
			pe := moduleGraphParseError{
				URI:     uri.FromPath(filepath.Join(modPath, string(filename))),
				Message: diag.Summary,
			}
			if diag.Detail != "" {
				pe.Message += ": " + diag.Detail
			}
			if diag.Subject != nil {
				pe.Range = ilsp.HCLRangeToLSP(*diag.Subject)
			}
			errs = append(errs, pe)
		}
	}
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].URI != errs[j].URI {
			return errs[i].URI < errs[j].URI
		}
		return errs[i].Range.Start.Line < errs[j].Range.Start.Line
	})
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func isLocalModuleSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		strings.HasPrefix(source, ".\\") || strings.HasPrefix(source, "..\\")
}

// graphBlock is a declaration found while scanning the files, together
// with the body (or expression, for a local) its references live in.
type graphBlock struct {
	node moduleGraphNode
	file *hcl.File
	body *hclsyntax.Body
	expr hclsyntax.Expression
}

func buildModuleGraph(modPath string, files map[string]*hcl.File, childFiles moduleGraphChildFiles) moduleGraphResponse {
	response := moduleGraphResponse{
		FormatVersion: moduleGraphVersion,
		ModuleURI:     uri.FromPath(modPath),
		Nodes:         make([]moduleGraphNode, 0),
		Edges:         make([]moduleGraphEdge, 0),
	}

	filenames := make([]string, 0, len(files))
	for name := range files {
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)

	blocks := make([]*graphBlock, 0)
	byID := make(map[string]*graphBlock)
	for _, name := range filenames {
		file := files[name]
		if file == nil || shadowedByTofuFile(name, files) {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			// JSON configuration has no traversals to follow
			continue
		}
		fileURI := uri.FromPath(filepath.Join(modPath, name))
		for _, gb := range declarationsInBody(body, file, fileURI) {
			if _, exists := byID[gb.node.ID]; exists {
				// duplicate declarations are an error reported elsewhere;
				// the first one wins here
				continue
			}
			byID[gb.node.ID] = gb
			blocks = append(blocks, gb)
		}
	}

	edges := make(map[[2]string]*moduleGraphEdge)
	edgeKeys := make([][2]string, 0)
	outputsUsed := make(map[string]map[string]bool)
	for _, gb := range blocks {
		fileURI := gb.node.URI
		addRef := func(attr string, traversal hcl.Traversal, providerRef bool) {
			targetID, ok := graphTargetID(traversal, providerRef)
			if !ok || targetID == gb.node.ID {
				return
			}
			if _, ok := byID[targetID]; !ok {
				return
			}
			if name, ok := moduleOutputName(traversal); ok && strings.HasPrefix(targetID, "module.") {
				if outputsUsed[targetID] == nil {
					outputsUsed[targetID] = make(map[string]bool)
				}
				outputsUsed[targetID][name] = true
			}
			key := [2]string{targetID, gb.node.ID}
			edge, ok := edges[key]
			if !ok {
				edge = &moduleGraphEdge{From: targetID, To: gb.node.ID, Refs: make([]moduleGraphRef, 0)}
				edges[key] = edge
				edgeKeys = append(edgeKeys, key)
			}
			rng := traversal.SourceRange()
			edge.Refs = append(edge.Refs, moduleGraphRef{
				URI:       fileURI,
				Range:     ilsp.HCLRangeToLSP(rng),
				Attribute: attr,
				Text:      string(rng.SliceBytes(gb.file.Bytes)),
			})
		}

		// a provider reference, such as aws.west, null.by_key (null is a
		// keyword) or random.by_key[var.k], whose key reads values
		addProviderRef := func(attrPath string, expr hclsyntax.Expression) {
			addr, key := providerReference(expr)
			if addr != nil {
				addRef(attrPath, addr, true)
			}
			if key != nil {
				for _, traversal := range hclsyntax.Variables(key) {
					addRef(attrPath, traversal, false)
				}
			}
		}
		addValueRefs := func(attrPath string, expr hclsyntax.Expression) {
			for _, traversal := range hclsyntax.Variables(expr) {
				addRef(attrPath, traversal, false)
			}
			// module.x[*].out and module.x[var.k].out read the output out,
			// which the variables of the expression (module.x) do not say
			for _, traversal := range instanceTraversals(expr) {
				if name, ok := moduleOutputName(traversal); ok {
					if targetID, ok := graphTargetID(traversal, false); ok && strings.HasPrefix(targetID, "module.") {
						if _, ok := byID[targetID]; ok {
							if outputsUsed[targetID] == nil {
								outputsUsed[targetID] = make(map[string]bool)
							}
							outputsUsed[targetID][name] = true
						}
					}
				}
			}
		}

		if gb.expr != nil {
			addValueRefs("", gb.expr)
			continue
		}
		walkGraphBody(gb.body, "", func(attrPath string, expr hclsyntax.Expression) {
			if gb.node.Kind == "module" && attrPath == "providers" {
				// the keys name the child's provider configurations
				// (aws.child = aws.parent); only the values are references here
				if obj, ok := expr.(*hclsyntax.ObjectConsExpr); ok {
					for _, item := range obj.Items {
						addProviderRef(attrPath, item.ValueExpr)
					}
					return
				}
			}
			if (gb.node.Kind == "resource" || gb.node.Kind == "data" || gb.node.Kind == "ephemeral") && attrPath == "provider" {
				addProviderRef(attrPath, expr)
				return
			}
			addValueRefs(attrPath, expr)
		})
	}

	for _, gb := range blocks {
		if gb.node.Kind == "module" {
			gb.node.Child = moduleGraphChildOf(gb, outputsUsed[gb.node.ID], childFiles)
		}
		response.Nodes = append(response.Nodes, gb.node)
	}

	sort.SliceStable(edgeKeys, func(i, j int) bool {
		if edgeKeys[i][0] != edgeKeys[j][0] {
			return edgeKeys[i][0] < edgeKeys[j][0]
		}
		return edgeKeys[i][1] < edgeKeys[j][1]
	})
	for _, key := range edgeKeys {
		edge := edges[key]
		sort.SliceStable(edge.Refs, func(i, j int) bool {
			a, b := edge.Refs[i], edge.Refs[j]
			if a.URI != b.URI {
				return a.URI < b.URI
			}
			if a.Range.Start.Line != b.Range.Start.Line {
				return a.Range.Start.Line < b.Range.Start.Line
			}
			return a.Range.Start.Character < b.Range.Start.Character
		})
		response.Edges = append(response.Edges, *edge)
	}

	return response
}

// declarationsInBody returns the top-level declarations of one file in
// source order.
func declarationsInBody(body *hclsyntax.Body, file *hcl.File, fileURI string) []*graphBlock {
	blocks := make([]*graphBlock, 0)
	for _, block := range body.Blocks {
		node := moduleGraphNode{
			URI:       fileURI,
			Range:     ilsp.HCLRangeToLSP(block.Range()),
			NameRange: ilsp.HCLRangeToLSP(block.DefRange()),
		}
		switch {
		case block.Type == "variable" && len(block.Labels) == 1:
			node.Kind, node.Name, node.ID = "variable", block.Labels[0], "var."+block.Labels[0]
			node.Description = literalString(block.Body, "description")
			node.Sensitive = literalBool(block.Body, "sensitive")
			if attr, ok := block.Body.Attributes["type"]; ok {
				node.Detail = oneLine(sourceText(file, attr.Expr.Range()))
			}
		case block.Type == "output" && len(block.Labels) == 1:
			node.Kind, node.Name, node.ID = "output", block.Labels[0], "output."+block.Labels[0]
			node.Description = literalString(block.Body, "description")
			node.Sensitive = literalBool(block.Body, "sensitive")
		case block.Type == "resource" && len(block.Labels) == 2:
			node.Kind, node.Type, node.Name = "resource", block.Labels[0], block.Labels[1]
			node.ID = block.Labels[0] + "." + block.Labels[1]
			node.Repeat = repeatMode(block.Body)
		case block.Type == "data" && len(block.Labels) == 2:
			node.Kind, node.Type, node.Name = "data", block.Labels[0], block.Labels[1]
			node.ID = "data." + block.Labels[0] + "." + block.Labels[1]
			node.Repeat = repeatMode(block.Body)
		case block.Type == "ephemeral" && len(block.Labels) == 2:
			node.Kind, node.Type, node.Name = "ephemeral", block.Labels[0], block.Labels[1]
			node.ID = "ephemeral." + block.Labels[0] + "." + block.Labels[1]
			node.Repeat = repeatMode(block.Body)
		case block.Type == "module" && len(block.Labels) == 1:
			node.Kind, node.Name, node.ID = "module", block.Labels[0], "module."+block.Labels[0]
			node.Detail = literalString(block.Body, "source")
			node.Repeat = repeatMode(block.Body)
		case block.Type == "provider" && len(block.Labels) == 1:
			node.Kind, node.Type = "provider", block.Labels[0]
			node.Name = block.Labels[0]
			if alias := literalString(block.Body, "alias"); alias != "" {
				node.Name = block.Labels[0] + "." + alias
			}
			node.ID = "provider." + node.Name
		case block.Type == "check" && len(block.Labels) == 1:
			// its assertions (and scoped data sources) use other
			// declarations, which must not look unused
			node.Kind, node.Name, node.ID = "check", block.Labels[0], "check."+block.Labels[0]
		case block.Type == "locals":
			for _, attr := range sortedAttributes(block.Body) {
				blocks = append(blocks, &graphBlock{
					node: moduleGraphNode{
						ID:        "local." + attr.Name,
						Kind:      "local",
						Name:      attr.Name,
						URI:       fileURI,
						Range:     ilsp.HCLRangeToLSP(attr.SrcRange),
						NameRange: ilsp.HCLRangeToLSP(attr.NameRange),
						Detail:    oneLine(sourceText(file, attr.Expr.Range())),
					},
					file: file,
					expr: attr.Expr,
				})
			}
			continue
		default:
			continue
		}
		blocks = append(blocks, &graphBlock{node: node, file: file, body: block.Body})
	}
	return blocks
}

// walkGraphBody calls fn for every attribute in body and its nested
// blocks, in source order, with the dotted path of the attribute.
func walkGraphBody(body *hclsyntax.Body, prefix string, fn func(attrPath string, expr hclsyntax.Expression)) {
	for _, attr := range sortedAttributes(body) {
		fn(prefix+attr.Name, attr.Expr)
	}
	for _, block := range body.Blocks {
		name := block.Type
		if block.Type == "dynamic" && len(block.Labels) == 1 {
			name = block.Labels[0]
		}
		walkGraphBody(block.Body, prefix+name+".", fn)
	}
}

func sortedAttributes(body *hclsyntax.Body) []*hclsyntax.Attribute {
	attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
	for _, attr := range body.Attributes {
		attrs = append(attrs, attr)
	}
	sort.Slice(attrs, func(i, j int) bool {
		return attrs[i].SrcRange.Start.Byte < attrs[j].SrcRange.Start.Byte
	})
	return attrs
}

// graphTargetID maps a traversal to the ID of the node it references.
// Iteration and context references (each, count, self, path,
// terraform) and dynamic block iterators have no node.
func graphTargetID(traversal hcl.Traversal, providerRef bool) (string, bool) {
	if len(traversal) == 0 || traversal.IsRelative() {
		return "", false
	}
	root := traversal.RootName()
	attrAt := func(i int) (string, bool) {
		if len(traversal) <= i {
			return "", false
		}
		step, ok := traversal[i].(hcl.TraverseAttr)
		if !ok {
			return "", false
		}
		return step.Name, true
	}

	if providerRef {
		if alias, ok := attrAt(1); ok {
			return "provider." + root + "." + alias, true
		}
		return "provider." + root, true
	}

	switch root {
	case "var", "local", "module":
		name, ok := attrAt(1)
		if !ok {
			return "", false
		}
		return root + "." + name, true
	case "data", "ephemeral":
		typ, ok := attrAt(1)
		if !ok {
			return "", false
		}
		name, ok := attrAt(2)
		if !ok {
			return "", false
		}
		return root + "." + typ + "." + name, true
	case "each", "count", "self", "path", "terraform":
		return "", false
	}
	name, ok := attrAt(1)
	if !ok {
		return "", false
	}
	return root + "." + name, true
}

// moduleOutputName returns the output a module reference reads:
// "url" for module.app.url and for module.app[0].url, module.app["a"].url
// or module.app[*].url.
func moduleOutputName(traversal hcl.Traversal) (string, bool) {
	for i := 2; i < len(traversal); i++ {
		switch step := traversal[i].(type) {
		case hcl.TraverseIndex, hcl.TraverseSplat:
			continue
		case hcl.TraverseAttr:
			return step.Name, true
		}
		break
	}
	return "", false
}

func moduleGraphChildOf(gb *graphBlock, outputsUsed map[string]bool, childFiles moduleGraphChildFiles) *moduleGraphChild {
	if childFiles == nil {
		return nil
	}
	dir, files := childFiles(gb.node.Name, gb.node.Detail)
	if dir == "" {
		return nil
	}
	variables := make(map[string]moduleGraphPort)
	outputs := make(map[string]moduleGraphPort)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body, ok := files[name].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		fileURI := uri.FromPath(filepath.Join(dir, name))
		for _, block := range body.Blocks {
			if len(block.Labels) != 1 {
				continue
			}
			rng := ilsp.HCLRangeToLSP(block.DefRange())
			port := moduleGraphPort{Name: block.Labels[0], URI: fileURI, Range: &rng}
			switch block.Type {
			case "variable":
				if _, ok := variables[port.Name]; !ok {
					variables[port.Name] = port
				}
			case "output":
				if _, ok := outputs[port.Name]; !ok {
					outputs[port.Name] = port
				}
			}
		}
	}

	child := &moduleGraphChild{
		URI:     uri.FromPath(dir),
		Inputs:  make([]moduleGraphPort, 0),
		Outputs: make([]moduleGraphPort, 0),
	}
	for _, attr := range sortedAttributes(gb.body) {
		if isModuleMetaArgument(attr.Name) {
			continue
		}
		port, ok := variables[attr.Name]
		if !ok {
			port = moduleGraphPort{Name: attr.Name}
		}
		child.Inputs = append(child.Inputs, port)
	}
	usedNames := make([]string, 0, len(outputsUsed))
	for name := range outputsUsed {
		usedNames = append(usedNames, name)
	}
	sort.Strings(usedNames)
	for _, name := range usedNames {
		port, ok := outputs[name]
		if !ok {
			port = moduleGraphPort{Name: name}
		}
		child.Outputs = append(child.Outputs, port)
	}
	return child
}

// providerReference splits a provider reference into the provider's
// address and the key of an instance, if any: aws.west, null.by_key
// (null parses as the keyword, which AbsTraversalForExpr turns back into
// a name) or random.by_key[var.k].
func providerReference(expr hclsyntax.Expression) (hcl.Traversal, hclsyntax.Expression) {
	var key hclsyntax.Expression
	if idx, ok := expr.(*hclsyntax.IndexExpr); ok {
		expr, key = idx.Collection, idx.Key
	}
	addr, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() {
		return nil, key
	}
	return addr, key
}

// instanceTraversals returns the whole traversals of the splats and the
// dynamic indexes in expr, such as module.app[*].url and
// module.app[var.k].url, which the syntax splits in two. The key of a
// splat or a dynamic index is left unknown.
func instanceTraversals(expr hclsyntax.Expression) []hcl.Traversal {
	traversals := make([]hcl.Traversal, 0)
	hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
		switch e := node.(type) {
		case *hclsyntax.SplatExpr:
			src, ok := e.Source.(*hclsyntax.ScopeTraversalExpr)
			if !ok {
				return nil
			}
			each, ok := e.Each.(*hclsyntax.RelativeTraversalExpr)
			if !ok {
				return nil
			}
			if _, ok := each.Source.(*hclsyntax.AnonSymbolExpr); !ok {
				return nil
			}
			tr := append(hcl.Traversal{}, src.Traversal...)
			tr = append(tr, hcl.TraverseSplat{SrcRange: e.MarkerRange})
			traversals = append(traversals, append(tr, each.Traversal...))
		case *hclsyntax.RelativeTraversalExpr:
			idx, ok := e.Source.(*hclsyntax.IndexExpr)
			if !ok {
				return nil
			}
			coll, ok := idx.Collection.(*hclsyntax.ScopeTraversalExpr)
			if !ok {
				return nil
			}
			tr := append(hcl.Traversal{}, coll.Traversal...)
			tr = append(tr, hcl.TraverseIndex{Key: cty.DynamicVal, SrcRange: idx.BracketRange})
			traversals = append(traversals, append(tr, e.Traversal...))
		}
		return nil
	})
	return traversals
}

// shadowedByTofuFile reports whether a .tf file (or .tf.json) has a .tofu
// copy (or .tofu.json), which OpenTofu reads instead of it.
func shadowedByTofuFile(name string, files map[string]*hcl.File) bool {
	for _, ext := range []string{".tf", ".tf.json"} {
		if strings.HasSuffix(name, ext) {
			tofu := strings.TrimSuffix(name, ext) + strings.Replace(ext, ".tf", ".tofu", 1)
			if f, ok := files[tofu]; ok && f != nil {
				return true
			}
		}
	}
	return false
}

func isModuleMetaArgument(name string) bool {
	switch name {
	case "source", "version", "providers", "count", "for_each", "depends_on":
		return true
	}
	return false
}

func repeatMode(body *hclsyntax.Body) string {
	if _, ok := body.Attributes["for_each"]; ok {
		return "for_each"
	}
	if _, ok := body.Attributes["count"]; ok {
		return "count"
	}
	return ""
}

// literalString returns the value of a constant string attribute, or ""
// when the attribute is missing or not a constant.
func literalString(body *hclsyntax.Body, name string) string {
	attr, ok := body.Attributes[name]
	if !ok {
		return ""
	}
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || !val.IsKnown() || val.IsNull() || val.Type() != cty.String {
		return ""
	}
	return val.AsString()
}

func literalBool(body *hclsyntax.Body, name string) bool {
	attr, ok := body.Attributes[name]
	if !ok {
		return false
	}
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || !val.IsKnown() || val.IsNull() || val.Type() != cty.Bool {
		return false
	}
	return val.True()
}

func sourceText(file *hcl.File, rng hcl.Range) string {
	if file == nil || rng.End.Byte > len(file.Bytes) || rng.Start.Byte > rng.End.Byte {
		return ""
	}
	return string(rng.SliceBytes(file.Bytes))
}

const moduleGraphDetailMax = 120

// oneLine collapses whitespace so an expression fits a tooltip line,
// and shortens it with an ellipsis past moduleGraphDetailMax runes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > moduleGraphDetailMax {
		s = string(r[:moduleGraphDetailMax-1]) + "…"
	}
	return s
}
