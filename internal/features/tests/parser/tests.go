// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package parser

import (
	"path/filepath"

	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	"github.com/opentofu/tofu-ls/internal/tofu/parser"
)

// ParseTestFiles parses the test files and mock data files in dir.
func ParseTestFiles(fs parser.FS, dir string) (ast.Files, ast.Diags, error) {
	files := make(ast.Files, 0)
	diags := make(ast.Diags, 0)

	dirEntries, err := fs.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}

	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !ast.IsTestFilename(name) && !ast.IsMockFilename(name) {
			continue
		}

		src, err := fs.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}

		filename := ast.Filename(name)
		f, pDiags := parser.ParseFile(src, filename)
		diags[filename] = pDiags
		if f != nil {
			files[filename] = f
		}
	}

	return files, diags, nil
}
