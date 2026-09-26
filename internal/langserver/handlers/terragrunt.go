// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// terragruntLinks returns the links of a Terragrunt file: included files,
// dependencies, local sources and files that functions read.
func (svc *service) terragruntLinks(doc *document.Document) []lang.Link {
	if svc.features == nil || svc.features.Terragrunt == nil {
		return nil
	}
	links := make([]lang.Link, 0)
	for _, link := range svc.features.Terragrunt.Links(doc.Dir.Path(), doc.Filename) {
		links = append(links, lang.Link{
			URI:     uri.FromPath(link.Target),
			Tooltip: link.Tooltip,
			Range:   link.Range,
		})
	}
	return links
}

// terragruntLinkTarget returns the file that the path at pos names, as a
// definition target.
func (svc *service) terragruntLinkTarget(doc *document.Document, pos hcl.Pos) (*decoder.ReferenceTarget, bool) {
	if svc.features == nil || svc.features.Terragrunt == nil {
		return nil, false
	}
	return svc.features.Terragrunt.LinkTarget(doc.Dir.Path(), doc.Filename, pos)
}
