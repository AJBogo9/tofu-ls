// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hooks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/requiredproviders"
	"github.com/zclconf/go-cty/cty"
)

// minRegistryMatches is how many matches the list of popular providers
// must have before the registry search is left out.
const minRegistryMatches = 5

type providerSourceCandidate struct {
	addr        string
	detail      string
	description string
	rank        int
	popularity  int64
	order       int
}

// ProviderSources completes the source of a required_providers entry
// with provider addresses: the providers the language server has schemas
// for whose type is the entry's name, then the registry's popular
// providers matching what is typed, then registry search results.
// Registry failures leave out the registry's candidates.
func (h *Hooks) ProviderSources(ctx context.Context, value cty.Value) ([]decoder.Candidate, error) {
	candidates := make([]decoder.Candidate, 0)

	entry, maxCandidates, err := h.requiredProviderEntry(ctx)
	if err != nil || entry == nil {
		return candidates, err
	}
	prefix := strings.ToLower(value.AsString())
	name := strings.ToLower(entry.Name)

	found := make(map[string]*providerSourceCandidate)
	order := 0
	add := func(c providerSourceCandidate) {
		key := strings.ToLower(c.addr)
		if prev, ok := found[key]; ok {
			if c.rank < prev.rank {
				prev.rank = c.rank
			}
			if prev.detail == "" {
				prev.detail = c.detail
			}
			if prev.description == "" {
				prev.description = c.description
			}
			if c.popularity > prev.popularity {
				prev.popularity = c.popularity
			}
			return
		}
		c.order = order
		order++
		found[key] = &c
	}

	// Providers with a schema here: installed or bundled
	if h.ProviderSchemas != nil {
		if it, err := h.ProviderSchemas.ListSchemas(); err == nil {
			for ps := it.Next(); ps != nil; ps = it.Next() {
				addr := ps.Address
				if addr.IsBuiltIn() || addr.IsLegacy() || addr.Type != name {
					continue
				}
				display := addr.ForDisplay()
				if !sourceMatches(display, prefix) {
					continue
				}
				detail := "schema available"
				if ps.Version != nil {
					detail = fmt.Sprintf("schema available (%s)", ps.Version)
				}
				add(providerSourceCandidate{addr: display, detail: detail, rank: 0})
			}
		}
	}

	registryMatches := 0
	if top, err := h.RegistryClient.TopProviders(ctx); err == nil {
		for _, p := range top {
			pAddr, err := tfaddr.ParseProviderSource(p.Addr)
			if err != nil || isLegacyMirror(pAddr) || !sourceMatches(p.Addr, prefix) {
				continue
			}
			rank := sourceRank(pAddr, name, prefix)
			if prefix == "" && rank > 3 {
				// nothing typed: only providers related to the name
				continue
			}
			registryMatches++
			add(providerSourceCandidate{
				addr:       p.Addr,
				detail:     strings.TrimPrefix(p.Version, "v"),
				rank:       rank,
				popularity: p.Popularity,
			})
		}
	} else if errors.Is(err, context.Canceled) {
		return candidates, nil
	}

	query := prefix
	if _, typ, ok := strings.Cut(prefix, "/"); ok {
		query = typ
	}
	if query == "" {
		query = name
	}
	if registryMatches < minRegistryMatches && len(query) >= 2 {
		if results, err := h.RegistryClient.SearchProviders(ctx, query); err == nil {
			for _, r := range results {
				pAddr, err := tfaddr.ParseProviderSource(r.Addr)
				if err != nil || isLegacyMirror(pAddr) || !sourceMatches(r.Addr, prefix) {
					continue
				}
				// below the popular providers of the same rank
				add(providerSourceCandidate{
					addr:        r.Addr,
					detail:      strings.TrimPrefix(r.Version, "v"),
					description: r.Description,
					rank:        sourceRank(pAddr, name, prefix) + 1,
				})
			}
		}
	}

	sorted := make([]*providerSourceCandidate, 0, len(found))
	for _, c := range found {
		sorted = append(sorted, c)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.popularity != b.popularity {
			return a.popularity > b.popularity
		}
		return a.order < b.order
	})

	for i, c := range sorted {
		if uint(i) >= maxCandidates {
			break
		}
		cand := decoder.ExpressionCompletionCandidate(decoder.ExpressionCandidate{
			Value:       cty.StringVal(c.addr),
			Detail:      c.detail,
			Description: lang.PlainText(c.description),
		})
		cand.SortText = fmt.Sprintf("%3d", i)
		candidates = append(candidates, cand)
	}

	return candidates, nil
}

// sourceMatches tells whether a provider address matches what is typed:
// the start of the address, or of its type.
func sourceMatches(addr, prefix string) bool {
	if prefix == "" {
		return true
	}
	addr = strings.ToLower(addr)
	if strings.HasPrefix(addr, prefix) {
		return true
	}
	if strings.Contains(prefix, "/") {
		return false
	}
	_, typ, _ := strings.Cut(addr, "/")
	return strings.HasPrefix(typ, prefix)
}

// sourceRank orders provider addresses for an entry: its type equal to
// the entry's name first, from the hashicorp namespace before others,
// then types starting with the name or with what is typed.
func sourceRank(addr tfaddr.Provider, name, prefix string) int {
	switch {
	case name != "" && addr.Type == name && addr.Namespace == "hashicorp":
		return 1
	case name != "" && addr.Type == name:
		return 2
	case name != "" && strings.HasPrefix(addr.Type, name):
		return 3
	case prefix != "" && strings.HasPrefix(addr.Namespace+"/"+addr.Type, prefix):
		return 3
	}
	return 5
}

// isLegacyMirror tells whether addr is in the terraform-providers
// namespace, which only mirrors providers for configurations older than
// provider source addresses.
func isLegacyMirror(addr tfaddr.Provider) bool {
	return addr.Namespace == "terraform-providers"
}

// requiredProviderEntry returns the required_providers entry at the
// completion position, or nil when there is none.
func (h *Hooks) requiredProviderEntry(ctx context.Context) (*requiredproviders.Entry, uint, error) {
	path, ok := decoder.PathFromContext(ctx)
	if !ok {
		return nil, 0, errors.New("missing context: path")
	}
	pos, ok := decoder.PosFromContext(ctx)
	if !ok {
		return nil, 0, errors.New("missing context: pos")
	}
	filename, ok := decoder.FilenameFromContext(ctx)
	if !ok {
		return nil, 0, errors.New("missing context: filename")
	}
	maxCandidates, ok := decoder.MaxCandidatesFromContext(ctx)
	if !ok {
		return nil, 0, errors.New("missing context: maxCandidates")
	}

	mod, err := h.ModStore.ModuleRecordByPath(path.Path)
	if err != nil {
		return nil, 0, err
	}
	file, ok := mod.ParsedModuleFiles[ast.ModFilename(filename)]
	if !ok {
		return nil, 0, nil
	}
	entry, ok := requiredproviders.EntryAtPos(file, filename, hcl.Pos{Line: pos.Line, Column: pos.Column, Byte: pos.Byte})
	if !ok {
		return nil, 0, nil
	}
	return &entry, maxCandidates, nil
}
