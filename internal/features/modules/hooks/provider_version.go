// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl-lang/decoder"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/zclconf/go-cty/cty"
)

// versionOperators are the version constraint operators, longest first so
// that a prefix match finds the whole operator.
var versionOperators = []string{"~>", ">=", "<=", "!=", ">", "<", "="}

// ProviderVersions completes the version constraint of a required_providers
// entry with the versions of the entry's provider in the registry, newest
// first. With nothing or only a version typed, it offers `~> MAJOR.MINOR`
// for each minor release and each exact version; after `~>`, the same
// with the operator; after another operator, that operator with each
// version. A version typed after the operator narrows the list. In a list
// of constraints, only the last one is completed. Pre-releases are left
// out unless a `-` is typed. A registry failure gives no candidates.
func (h *Hooks) ProviderVersions(ctx context.Context, value cty.Value) ([]decoder.Candidate, error) {
	candidates := make([]decoder.Candidate, 0)

	entry, maxCandidates, err := h.requiredProviderEntry(ctx)
	if err != nil || entry == nil {
		return candidates, err
	}

	source := entry.Source
	if source == "" {
		// OpenTofu implies the hashicorp namespace
		source = "hashicorp/" + entry.Name
	}
	addr, err := tfaddr.ParseProviderSource(source)
	if err != nil {
		return candidates, nil
	}
	versions, err := h.RegistryClient.ProviderVersions(ctx, addr)
	if err != nil {
		return candidates, nil
	}

	typed := value.AsString()
	head := ""
	if i := strings.LastIndex(typed, ","); i >= 0 {
		head = typed[:i+1] + " "
		typed = typed[i+1:]
	}
	typed = strings.TrimSpace(typed)
	operator := ""
	for _, op := range versionOperators {
		if strings.HasPrefix(typed, op) {
			operator = op
			break
		}
	}
	withPrereleases := strings.Contains(typed, "-")
	// the version typed so far narrows the list, so that older versions
	// are reachable despite the cap on candidates
	typedVersion := strings.TrimSpace(strings.TrimPrefix(typed, operator))

	texts := make([]string, 0)
	seenMinor := make(map[string]bool)
	for _, v := range versions {
		if v.Prerelease() != "" && !withPrereleases {
			continue
		}
		if !strings.HasPrefix(v.String(), typedVersion) {
			continue
		}
		minor := minorVersion(v)
		if (operator == "" || operator == "~>") && v.Prerelease() == "" && !seenMinor[minor] {
			texts = append(texts, "~> "+minor)
			seenMinor[minor] = true
		}
		switch operator {
		case "":
			texts = append(texts, v.String())
		default:
			texts = append(texts, operator+" "+v.String())
		}
	}

	for i, text := range texts {
		if uint(i) >= maxCandidates {
			break
		}
		c := decoder.ExpressionCompletionCandidate(decoder.ExpressionCandidate{
			Value: cty.StringVal(head + text),
		})
		if i == 0 {
			c.Detail = fmt.Sprintf("newest %s", addr.ForDisplay())
		}
		// hcl-lang caps candidates at 100, so three digits sort them
		c.SortText = fmt.Sprintf("%3d", i)
		candidates = append(candidates, c)
	}

	return candidates, nil
}

func minorVersion(v *version.Version) string {
	segments := v.Segments()
	return fmt.Sprintf("%d.%d", segments[0], segments[1])
}
