// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/hcl-lang/validator"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/paths"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

// read-only and shared by all Terragrunt path contexts
var (
	unitSchema           = tfschema.TerragruntFileSchema()
	stackSchema          = tfschema.TerragruntStackFileSchema()
	functions            = tfschema.TerragruntFunctions()
	semanticHighlighting = tfschema.TerragruntSemanticHighlighting()
)

// Validators report the blocks and attributes that the schema does not
// have. Terragrunt changes often, so the job turns them into warnings.
var Validators = []validator.Validator{
	validator.UnexpectedAttribute{},
	validator.UnexpectedBlock{},
}

type StateReader interface {
	List() ([]*state.TerragruntRecord, error)
	TerragruntRecordByPath(path string) (*state.TerragruntRecord, error)
}

type PathReader struct {
	StateReader StateReader
	// FS reads the files that the Terragrunt files include. Without it,
	// the targets in included files are left out.
	FS paths.FS
	// Includes caches the parsed included files; it may be nil.
	Includes *IncludeCache
}

var _ decoder.PathReader = &PathReader{}
var _ decoder.ReferencePathReader = &PathReader{}

// Paths returns a path per language: one for the unit and root files and
// one for the stack files of each directory which has them.
func (pr *PathReader) Paths(ctx context.Context) []lang.Path {
	paths := make([]lang.Path, 0)

	records, err := pr.StateReader.List()
	if err != nil {
		return paths
	}

	for _, record := range records {
		for _, language := range []ilsp.LanguageID{ilsp.Terragrunt, ilsp.TerragruntStack} {
			if record.HasLanguage(language.String()) {
				paths = append(paths, lang.Path{Path: record.Path(), LanguageID: language.String()})
			}
		}
	}

	return paths
}

func schemaOf(languageID string) (*schema.BodySchema, error) {
	switch ilsp.LanguageID(languageID) {
	case ilsp.Terragrunt:
		return unitSchema, nil
	case ilsp.TerragruntStack:
		return stackSchema, nil
	}
	return nil, fmt.Errorf("unknown language ID for Terragrunt files: %q", languageID)
}

// PathContext returns the context of the unit and root files, or of the
// stack files, of a directory.
func (pr *PathReader) PathContext(path lang.Path) (*decoder.PathContext, error) {
	pathCtx, err := pr.ReferencePathContext(path)
	if err != nil {
		return nil, err
	}
	pathCtx.Schema, _ = schemaOf(path.LanguageID)
	pathCtx.Functions = functions
	pathCtx.Validators = Validators
	pathCtx.SemanticHighlighting = semanticHighlighting
	return pathCtx, nil
}

// ReferencePathContext returns the reference origins and targets and the
// files of the path, without the schema (see decoder.ReferencePathReader).
func (pr *PathReader) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	if _, err := schemaOf(path.LanguageID); err != nil {
		return nil, err
	}
	record, err := pr.StateReader.TerragruntRecordByPath(path.Path)
	if err != nil {
		return nil, err
	}

	files := record.FilesOf(path.LanguageID)
	pathCtx := &decoder.PathContext{
		ReferenceOrigins: make(reference.Origins, 0),
		ReferenceTargets: make(reference.Targets, 0),
		Files:            files,
	}
	for _, origin := range record.RefOrigins {
		if _, ok := files[origin.OriginRange().Filename]; ok {
			pathCtx.ReferenceOrigins = append(pathCtx.ReferenceOrigins, origin)
		}
	}
	for _, target := range record.RefTargets {
		if target.RangePtr != nil && files[target.RangePtr.Filename] != nil {
			pathCtx.ReferenceTargets = append(pathCtx.ReferenceTargets, target)
		}
	}
	if pr.FS != nil {
		pathCtx.ReferenceTargets = append(pathCtx.ReferenceTargets,
			IncludeTargets(pr.FS, pr.Includes, record.Path(), files)...)
	}

	return pathCtx, nil
}
