// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/langserver/cmd"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/requiredproviders"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// addRequiredProviderCommand declares a provider in required_providers of
// another file than the one being completed, which a completion item's
// additionalTextEdits cannot edit.
const addRequiredProviderCommand = "module.addRequiredProvider"

// moduleFiles returns the parsed configuration files of the module in dir.
func (svc *service) moduleFiles(dir string) (map[string]*hcl.File, bool) {
	if svc.features == nil || svc.features.Modules == nil {
		return nil, false
	}
	mod, err := svc.features.Modules.Store.ModuleRecordByPath(dir)
	if err != nil || len(mod.ParsedModuleFiles) == 0 {
		return nil, false
	}
	files := make(map[string]*hcl.File, len(mod.ParsedModuleFiles))
	for name, f := range mod.ParsedModuleFiles {
		if name.IsIgnored() {
			continue
		}
		files[name.String()] = f
	}
	return files, true
}

// providerSourceFor returns the source address to declare for a provider
// local name: the address of a provider schema of that type which the
// language server has, installed or bundled, preferring the hashicorp
// namespace as OpenTofu implies it; else hashicorp/<name>.
func (svc *service) providerSourceFor(name string) string {
	source := "hashicorp/" + name
	if svc.stateStore == nil || svc.stateStore.ProviderSchemas == nil {
		return source
	}
	it, err := svc.stateStore.ProviderSchemas.ListSchemas()
	if err != nil {
		return source
	}
	var others []string
	for ps := it.Next(); ps != nil; ps = it.Next() {
		addr := ps.Address
		if addr.Type != name || addr.IsBuiltIn() || addr.IsLegacy() || ps.Schema == nil {
			continue
		}
		if addr.Namespace == "hashicorp" {
			return addr.ForDisplay()
		}
		others = append(others, addr.ForDisplay())
	}
	if len(others) > 0 {
		sort.Strings(others)
		return others[0]
	}
	return source
}

// providerCompletionExtras adds what the schema cannot know to the
// candidates of a completion in a configuration file:
//
//   - inside required_providers, the local names of providers that the
//     module uses and does not declare yet;
//   - on the type label of a resource, data source or ephemeral
//     resource, an edit that declares the type's provider in
//     required_providers (opentofu.completion.addRequiredProviders).
//
// A declaration in the document being completed goes into the candidate's
// additional edits. One in another file needs a command, returned by the
// index of its candidate. entry reports a new required_providers entry's
// position, where a half-typed name may keep the schema from giving any
// candidates.
func (svc *service) providerCompletionExtras(ctx context.Context, doc *document.Document, pos hcl.Pos, candidates *lang.Candidates) (commands map[int]*lsp.Command, entry bool) {
	if ilsp.ParseLanguageID(doc.LanguageID) != ilsp.OpenTofu {
		return nil, false
	}
	files, ok := svc.moduleFiles(doc.Dir.Path())
	if !ok {
		return nil, false
	}
	file, ok := files[doc.Filename]
	if !ok {
		return nil, false
	}

	// reading the tokens costs a pass over the file, which most files
	// can skip
	if bytes.Contains(doc.Text, []byte("required_providers")) && requiredproviders.NewEntryAtPos(doc.Text, doc.Filename, pos) {
		candidates.List = append(svc.localNameCandidates(files, doc.Text, pos), candidates.List...)
		return nil, true
	}

	if !svc.completionOptions.AddRequiredProviders {
		return nil, false
	}
	block, ok := typeLabelBlockAtPos(file, pos)
	if !ok {
		return nil, false
	}
	declared := requiredproviders.Entries(files)

	type declaration struct {
		edit    requiredproviders.Edit
		command *lsp.Command
		ok      bool
	}
	byName := make(map[string]declaration)
	commands = make(map[int]*lsp.Command)
	for i, c := range candidates.List {
		if c.Kind != lang.LabelCandidateKind {
			continue
		}
		name, ok := requiredproviders.ResourceLocalName(c.Label, block.Body)
		if !ok || name == "terraform" {
			continue
		}
		if _, ok := declared[name]; ok {
			continue
		}
		decl, seen := byName[name]
		if !seen {
			source := svc.providerSourceFor(name)
			edit, ok := requiredproviders.AddEntryEdit(files, doc.Filename, name, source, requiredproviders.EntryOptions{})
			decl = declaration{edit: edit, ok: ok}
			if ok && edit.Filename != doc.Filename {
				decl.command = svc.addRequiredProviderCommand(ctx, doc, name, source)
				decl.ok = decl.command != nil
			}
			byName[name] = decl
		}
		if !decl.ok {
			continue
		}
		if decl.command != nil {
			commands[i] = decl.command
			continue
		}
		rng := ilsp.HCLRangeToLSPInText(decl.edit.Range, doc.Text)
		candidates.List[i].AdditionalTextEdits = append(candidates.List[i].AdditionalTextEdits, lang.TextEdit{
			Range:   lspRangeToHCL(rng, decl.edit.Range),
			NewText: decl.edit.NewText,
			Snippet: decl.edit.NewText,
		})
	}
	return commands, false
}

// lspRangeToHCL keeps rng's lines and bytes, with the columns of the LSP
// range, so that converting it back to LSP gives the UTF-16 columns.
func lspRangeToHCL(lspRng lsp.Range, rng hcl.Range) hcl.Range {
	rng.Start.Column = int(lspRng.Start.Character) + 1
	rng.End.Column = int(lspRng.End.Character) + 1
	return rng
}

// addRequiredProviderCommand returns the command which declares name in
// required_providers, or nil when the client cannot apply the edit.
func (svc *service) addRequiredProviderCommand(ctx context.Context, doc *document.Document, name, source string) *lsp.Command {
	cc, err := ilsp.ClientCapabilities(ctx)
	if err != nil || !cc.Workspace.ApplyEdit {
		return nil
	}
	commandPrefix, _ := lsctx.CommandPrefix(ctx)
	command := cmd.Name(addRequiredProviderCommand)
	if commandPrefix != "" {
		command = commandPrefix + "." + command
	}
	docURI := uri.FromPath(filepath.Join(doc.Dir.Path(), doc.Filename))
	var arguments []json.RawMessage
	for _, arg := range []string{"uri=" + docURI, "name=" + name, "source=" + source} {
		raw, err := json.Marshal(arg)
		if err != nil {
			return nil
		}
		arguments = append(arguments, raw)
	}
	return &lsp.Command{
		Title:     fmt.Sprintf("Declare provider %s in required_providers", name),
		Command:   command,
		Arguments: arguments,
	}
}

// addRequiredProviderHandler declares a provider in the module's
// required_providers through workspace/applyEdit, unless it is declared
// already.
func (svc *service) addRequiredProviderHandler(ctx context.Context, args cmd.CommandArgs) (any, error) {
	docURI, ok := args.GetString("uri")
	if !ok || docURI == "" {
		return nil, fmt.Errorf("%w: expected uri argument to be set", jrpc2.InvalidParams.Err())
	}
	name, ok := args.GetString("name")
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: expected name argument to be set", jrpc2.InvalidParams.Err())
	}
	source, ok := args.GetString("source")
	if !ok || source == "" {
		return nil, fmt.Errorf("%w: expected source argument to be set", jrpc2.InvalidParams.Err())
	}
	path, err := uri.PathFromURI(docURI)
	if err != nil {
		return nil, err
	}
	dir, filename := filepath.Dir(path), filepath.Base(path)

	files, ok := svc.moduleFiles(dir)
	if !ok {
		return nil, nil
	}
	params, ok := addRequiredProviderEdit(files, dir, filename, name, source)
	if !ok {
		return nil, nil
	}
	_, err = svc.server.Callback(ctx, "workspace/applyEdit", params)
	return nil, err
}

// addRequiredProviderEdit returns the workspace edit which declares name in
// the required_providers of the module in dir, whose files are files,
// unless it is declared already.
func addRequiredProviderEdit(files map[string]*hcl.File, dir, filename, name, source string) (lsp.ApplyWorkspaceEditParams, bool) {
	if _, ok := requiredproviders.Entries(files)[name]; ok {
		return lsp.ApplyWorkspaceEditParams{}, false
	}
	edit, ok := requiredproviders.AddEntryEdit(files, filename, name, source, requiredproviders.EntryOptions{})
	if !ok {
		return lsp.ApplyWorkspaceEditParams{}, false
	}
	target := files[edit.Filename]
	targetURI := uri.FromPath(filepath.Join(dir, edit.Filename))
	return lsp.ApplyWorkspaceEditParams{
		Label: fmt.Sprintf("Declare provider %s", name),
		Edit: lsp.WorkspaceEdit{
			Changes: map[lsp.DocumentURI][]lsp.TextEdit{
				lsp.DocumentURI(targetURI): {
					{
						Range:   ilsp.HCLRangeToLSPInText(edit.Range, target.Bytes),
						NewText: edit.NewText,
					},
				},
			},
		},
	}, true
}

// localNameCandidates returns, for a position inside required_providers
// where an entry can start, an entry for each provider local name that
// the module uses and does not declare, with the name typed so far as
// prefix.
func (svc *service) localNameCandidates(files map[string]*hcl.File, src []byte, pos hcl.Pos) []lang.Candidate {
	declared := requiredproviders.Entries(files)
	start, end := requiredproviders.IdentAround(src, pos.Byte)
	prefix := string(src[start:pos.Byte])
	rng := hcl.Range{
		Start: hcl.Pos{Line: pos.Line, Column: pos.Column - (pos.Byte - start), Byte: start},
		End:   hcl.Pos{Line: pos.Line, Column: pos.Column + (end - pos.Byte), Byte: end},
	}

	candidates := make([]lang.Candidate, 0)
	for _, name := range requiredproviders.UsedLocalNames(files) {
		if _, ok := declared[name]; ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		source := svc.providerSourceFor(name)
		candidates = append(candidates, lang.Candidate{
			Label:       name,
			Detail:      source,
			Description: lang.Markdown(fmt.Sprintf("Declare the provider `%s`, which this module uses, as `%s`.", name, source)),
			Kind:        lang.AttributeCandidateKind,
			TextEdit: lang.TextEdit{
				Range:   rng,
				NewText: fmt.Sprintf("%s = {\n  source = %q\n}", name, source),
				Snippet: fmt.Sprintf("%s = {\n\tsource  = %q\n\tversion = \"${1}\"\n}", name, source),
			},
			// the version completes next
			TriggerSuggest: true,
			SortText:       "0" + name,
		})
	}
	return candidates
}

// typeLabelBlockAtPos returns the resource, data or ephemeral block whose
// type label holds pos, including data blocks nested in check blocks.
func typeLabelBlockAtPos(file *hcl.File, pos hcl.Pos) (*hclsyntax.Block, bool) {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, false
	}
	var find func(blocks hclsyntax.Blocks) (*hclsyntax.Block, bool)
	find = func(blocks hclsyntax.Blocks) (*hclsyntax.Block, bool) {
		for _, block := range blocks {
			if !block.Range().ContainsPos(pos) && block.Range().End.Byte != pos.Byte {
				continue
			}
			switch block.Type {
			case "resource", "data", "ephemeral":
				if len(block.LabelRanges) > 0 {
					rng := block.LabelRanges[0]
					if rng.ContainsPos(pos) || rng.End.Byte == pos.Byte {
						return block, true
					}
				}
			case "check":
				return find(block.Body.Blocks)
			}
		}
		return nil, false
	}
	return find(body.Blocks)
}
