// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	tfmod "github.com/opentofu/opentofu-schema/module"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
)

type StateReader interface {
	List() ([]*state.TestRecord, error)
	TestRecordByPath(path string) (*state.TestRecord, error)
}

// ModuleReader reads the modules which the runs of test files run.
type ModuleReader interface {
	LocalModuleMeta(modPath string) (*tfmod.Meta, error)
	PathContext(path lang.Path) (*decoder.PathContext, error)
	ReferencePathContext(path lang.Path) (*decoder.PathContext, error)
}

type PathReader struct {
	StateReader  StateReader
	ModuleReader ModuleReader

	// UseStaticSchema builds contexts from the schema of the test
	// language alone, without any module, e.g. to collect the reference
	// origins as they are written.
	UseStaticSchema bool
}

var _ decoder.PathReader = &PathReader{}
var _ decoder.ReferencePathReader = &PathReader{}

// Paths returns a path per language: one for the test files and one for
// the mock data files of each directory which has them.
func (pr *PathReader) Paths(ctx context.Context) []lang.Path {
	paths := make([]lang.Path, 0)

	records, err := pr.StateReader.List()
	if err != nil {
		return paths
	}

	for _, record := range records {
		if record.HasTestFiles() {
			paths = append(paths, lang.Path{Path: record.Path(), LanguageID: ilsp.OpenTofuTest.String()})
		}
		if record.HasMockFiles() {
			paths = append(paths, lang.Path{Path: record.Path(), LanguageID: ilsp.OpenTofuMock.String()})
		}
	}

	return paths
}

// PathContext returns the context of the test files or of the mock data
// files of a directory, depending on the language ID.
func (pr *PathReader) PathContext(path lang.Path) (*decoder.PathContext, error) {
	record, err := pr.StateReader.TestRecordByPath(path.Path)
	if err != nil {
		return nil, err
	}

	switch ilsp.ParseLanguageID(path.LanguageID) {
	case ilsp.OpenTofuTest:
		if pr.UseStaticSchema || pr.ModuleReader == nil {
			return staticTestPathContext(record), nil
		}
		return testPathContext(record, pr.ModuleReader, true), nil
	case ilsp.OpenTofuMock:
		return mockPathContext(record), nil
	}

	return nil, fmt.Errorf("unknown language ID for test files: %q", path.LanguageID)
}

// ReferencePathContext returns the reference origins and targets and the
// files of the test files, without their schema, for reference lookups
// across paths (see decoder.ReferencePathReader).
func (pr *PathReader) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	record, err := pr.StateReader.TestRecordByPath(path.Path)
	if err != nil {
		return nil, err
	}

	switch ilsp.ParseLanguageID(path.LanguageID) {
	case ilsp.OpenTofuTest:
		if pr.ModuleReader == nil {
			return staticTestPathContext(record), nil
		}
		return testPathContext(record, pr.ModuleReader, false), nil
	case ilsp.OpenTofuMock:
		return mockPathContext(record), nil
	}

	return nil, fmt.Errorf("unknown language ID for test files: %q", path.LanguageID)
}
