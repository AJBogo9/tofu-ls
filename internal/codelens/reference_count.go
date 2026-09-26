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
	"strings"

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

		reader, err := decoder.PathReaderFromContext(ctx)
		if err != nil {
			return nil, err
		}
		// Counting asks for the context of every path once per target;
		// building one is not free, so they are built once per request.
		pathReader := newCachedPathReader(reader)

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
			if isIterationTarget(refTarget) {
				// each.key, each.value and count.index: a lens on for_each
				// or count counts the block's own iteration symbols, which
				// says little and adds a line to the block
				continue
			}
			rng := *refTarget.RangePtr
			if _, ok := dedupedTargets[rng]; !ok {
				dedupedTargets[rng] = make(reference.Targets, 0)
			}
			dedupedTargets[rng] = append(dedupedTargets[rng], refTarget)
		}

		// variables and locals no expression uses, read from the syntax
		var unused map[string]bool
		unusedInSyntax := func() map[string]bool {
			if unused == nil {
				unused = unusedNames(localCtx.Files)
			}
			return unused
		}

		for rng, refTargets := range dedupedTargets {
			if err := ctx.Err(); err != nil {
				// the request was cancelled, for example by an edit
				return nil, err
			}
			// targets sharing a range can be reached by the same origin,
			// which counts once
			seen := make(map[originKey]string)
			var defRange *hcl.Range
			for _, refTarget := range refTargets {
				if refTarget.DefRangePtr != nil {
					defRange = refTarget.DefRangePtr
				}

				// resolved origins only: e.g. local.x does not count
				// towards a provider named "local"
				for _, po := range decoder.OriginsTargeting(ctx, pathReader, refTarget, path) {
					if ilsp.IsValidTestLanguage(po.Path.LanguageID) {
						// the lens counts the uses in configuration, as the
						// unused hint does; Find References lists the tests too
						continue
					}
					seen[originKey{path: po.Path, rng: po.Origin.OriginRange()}] = originKind(po, refTarget, path)
				}
			}
			originCount := len(seen)

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
					Title: lensTitle(seen, refTargets[0].Addr.String(), unusedInSyntax),
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

// Kinds of references that set or check a variable without using its
// value in an expression, which the unused hint does not count either.
const (
	originUse        = ""
	originVarsFile   = "tfvars"
	originModuleArg  = "module arguments"
	originValidation = "own validation"
)

// originKind classifies an origin of a lens's target. Only variables
// have references which are not uses: an output's references from
// module calls (module.x.out) are uses.
func originKind(po decoder.PathOrigin, target reference.Target, path lang.Path) string {
	if len(target.Addr) == 0 || target.Addr[0].String() != "var" {
		return originUse
	}
	switch po.Origin.(type) {
	case reference.PathOrigin:
		if po.Path.LanguageID != path.LanguageID {
			return originVarsFile
		}
		return originModuleArg
	case reference.LocalOrigin:
		rng := po.Origin.OriginRange()
		if po.Path.Equals(path) && target.RangePtr != nil && target.RangePtr.Filename == rng.Filename &&
			target.RangePtr.ContainsOffset(rng.Start.Byte) {
			// var.x in the validation of variable "x"
			return originValidation
		}
	}
	return originUse
}

// splitTitle counts a variable's references by kind when some of them
// are not uses in expressions: "6 uses · 12 callers", "0 uses · 1 tfvars".
// Uses inside the module come first, then the module calls that set it,
// tfvars assignments and its own validation. It returns "" when
// every reference is a use. The parts add up to what a click lists, and
// "0 uses" agrees with a faded "declared but not used" name.
func splitTitle(kinds map[originKey]string) string {
	counts := make(map[string]int)
	for _, kind := range kinds {
		counts[kind]++
	}
	if counts[originUse] == len(kinds) {
		return ""
	}
	parts := []string{getTitle("use", "uses", counts[originUse])}
	if n := counts[originModuleArg]; n > 0 {
		parts = append(parts, getTitle("caller", "callers", n))
	}
	if n := counts[originVarsFile]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d tfvars", n))
	}
	if n := counts[originValidation]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d in validation", n))
	}
	return strings.Join(parts, " · ")
}

// lensTitle is the title of a lens whose target (addr) has the origins
// kinds: "3 references", or splitTitle's counts by kind. The split is left
// out when it would say "0 uses" but the module's syntax uses the symbol
// somewhere the decoder does not see, such as a block without a schema.
func lensTitle(kinds map[originKey]string, addr string, unused func() map[string]bool) string {
	total := getTitle("reference", "references", len(kinds))
	split := splitTitle(kinds)
	if split == "" {
		return total
	}
	for _, kind := range kinds {
		if kind == originUse {
			return split
		}
	}
	if unused()[addr] {
		return split
	}
	return total
}

// isIterationTarget reports whether a target is each.key, each.value or
// count.index, which for_each and count declare for their block.
func isIterationTarget(t reference.Target) bool {
	if len(t.Addr) > 0 || len(t.LocalAddr) == 0 {
		return false
	}
	switch t.LocalAddr[0].String() {
	case "each", "count":
		return true
	}
	return false
}

// originKey identifies an origin across the targets of one lens.
type originKey struct {
	path lang.Path
	rng  hcl.Range
}

// cachedPathReader remembers the paths and path contexts it has read.
// It is meant for one request, during which the indexed modules do not
// change.
type cachedPathReader struct {
	reader  decoder.PathReader
	paths   []lang.Path
	ctxs    map[lang.Path]cachedPathContext
	refCtxs map[lang.Path]cachedPathContext
}

type cachedPathContext struct {
	pathCtx *decoder.PathContext
	err     error
}

func newCachedPathReader(reader decoder.PathReader) *cachedPathReader {
	return &cachedPathReader{
		reader:  reader,
		ctxs:    make(map[lang.Path]cachedPathContext),
		refCtxs: make(map[lang.Path]cachedPathContext),
	}
}

func (r *cachedPathReader) Paths(ctx context.Context) []lang.Path {
	if r.paths == nil {
		r.paths = r.reader.Paths(ctx)
	}
	return r.paths
}

func (r *cachedPathReader) PathContext(path lang.Path) (*decoder.PathContext, error) {
	if c, ok := r.ctxs[path]; ok {
		return c.pathCtx, c.err
	}
	pathCtx, err := r.reader.PathContext(path)
	r.ctxs[path] = cachedPathContext{pathCtx: pathCtx, err: err}
	return pathCtx, err
}

// ReferencePathContext lets decoder.OriginsTargeting read the reference
// contexts of the underlying reader, which skip the schema.
func (r *cachedPathReader) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	if c, ok := r.refCtxs[path]; ok {
		return c.pathCtx, c.err
	}
	pathCtx, err := decoder.ReferencePathContext(r.reader, path)
	r.refCtxs[path] = cachedPathContext{pathCtx: pathCtx, err: err}
	return pathCtx, err
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
