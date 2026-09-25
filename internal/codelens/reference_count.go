// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package codelens

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/refactor"
)

func ReferenceCount(showReferencesCmdId string) lang.CodeLensFunc {
	return func(ctx context.Context, path lang.Path, file string) ([]lang.CodeLens, error) {
		lenses := make([]lang.CodeLens, 0)

		localCtx, err := decoder.PathCtx(ctx)
		if err != nil {
			return nil, err
		}

		pathReader, err := decoder.PathReaderFromContext(ctx)
		if err != nil {
			return nil, err
		}

		refTargets := localCtx.ReferenceTargets.OutermostInFile(file)
		if err != nil {
			return nil, err
		}

		// There can be two targets pointing to the same range
		// e.g. when a block is targetable as type-less reference
		// and as an object, which is important in most contexts
		// but not here, where we present it to the user.
		dedupedTargets := make(map[hcl.Range]reference.Targets, 0)
		for _, refTarget := range refTargets {
			rng := *refTarget.RangePtr
			if _, ok := dedupedTargets[rng]; !ok {
				dedupedTargets[rng] = make(reference.Targets, 0)
			}
			dedupedTargets[rng] = append(dedupedTargets[rng], refTarget)
		}

		// variables and locals no expression uses, read from the syntax
		var unused map[string]bool

		for rng, refTargets := range dedupedTargets {
			originCount := 0
			var defRange *hcl.Range
			for _, refTarget := range refTargets {
				if refTarget.DefRangePtr != nil {
					defRange = refTarget.DefRangePtr
				}

				// resolved origins only: e.g. local.x does not count
				// towards a provider named "local"
				originCount += len(decoder.OriginsTargeting(ctx, pathReader, refTarget, path))
			}

			if originCount == 0 {
				if !showZeroReferences(refTargets) {
					continue
				}
				if unused == nil {
					unused = unusedNames(localCtx.Files)
				}
				if !unused[refTargets[0].Addr.String()] {
					// the syntax has a use the decoder did not see, so
					// "0 references" would be wrong
					continue
				}
			}

			var hclPos hcl.Pos
			if defRange != nil {
				hclPos = posMiddleOfRange(defRange)
			} else {
				hclPos = posMiddleOfRange(&rng)
			}

			lenses = append(lenses, lang.CodeLens{
				Range: rng,
				Command: lang.Command{
					Title: getTitle("reference", "references", originCount),
					ID:    showReferencesCmdId,
					Arguments: []lang.CommandArgument{
						Position(ilsp.HCLPosToLSP(hclPos)),
						ReferenceContext(lsp.ReferenceContext{}),
					},
				},
			})
		}

		sort.SliceStable(lenses, func(i, j int) bool {
			return lenses[i].Range.Start.Byte < lenses[j].Range.Start.Byte
		})

		return lenses, nil
	}
}

// showZeroReferences tells whether "0 references" is worth showing:
// an unreferenced variable or local value is dead code, while nothing
// references an output of a root module or a resource in normal use.
func showZeroReferences(targets reference.Targets) bool {
	for _, target := range targets {
		if len(target.Addr) != 2 {
			continue
		}
		root := target.Addr[0].String()
		if (root == "var" && target.ScopeId == "variable") ||
			(root == "local" && target.ScopeId == "local") {
			return true
		}
	}
	return false
}

// unusedNames returns the addresses (var.x, local.x) of the variables
// and locals which no expression of the module uses.
func unusedNames(files map[string]*hcl.File) map[string]bool {
	names := make(map[string]bool, 0)
	for _, sym := range refactor.UnusedSymbols(files) {
		switch sym.Kind {
		case refactor.KindVariable:
			names["var."+sym.Name] = true
		case refactor.KindLocal:
			names["local."+sym.Name] = true
		}
	}
	return names
}

type Position lsp.Position

func (p Position) MarshalJSON() ([]byte, error) {
	return json.Marshal(lsp.Position(p))
}

type ReferenceContext lsp.ReferenceContext

func (rc ReferenceContext) MarshalJSON() ([]byte, error) {
	return json.Marshal(lsp.ReferenceContext(rc))
}

func posMiddleOfRange(rng *hcl.Range) hcl.Pos {
	col := rng.Start.Column
	byte := rng.Start.Byte

	if rng.Start.Line == rng.End.Line && rng.End.Column > rng.Start.Column {
		charsFromStart := (rng.End.Column - rng.Start.Column) / 2
		col += charsFromStart
		byte += charsFromStart
	}

	return hcl.Pos{
		Line:   rng.Start.Line,
		Column: col,
		Byte:   byte,
	}
}

func getTitle(singular, plural string, n int) string {
	if n > 1 || n == 0 {
		return fmt.Sprintf("%d %s", n, plural)
	}
	return fmt.Sprintf("%d %s", n, singular)
}
