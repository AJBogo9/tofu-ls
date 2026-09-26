// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package parser

import (
	"path/filepath"

	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	"github.com/opentofu/tofu-ls/internal/tofu/parser"
)

// ParseTerragruntFiles parses the Terragrunt files in dir: the files with
// a Terragrunt name (see ast.LanguageOfFilename), and the files which are
// open as Terragrunt files (openLanguages, by name), which may have any
// name.
func ParseTerragruntFiles(fs parser.FS, dir string, openLanguages map[string]string) (ast.Files, ast.Languages, ast.Diags, error) {
	files := make(ast.Files, 0)
	languages := make(ast.Languages, 0)
	diags := make(ast.Diags, 0)

	dirEntries, err := fs.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}

	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		language := ast.LanguageOfFilename(name)
		if openLanguage, ok := openLanguages[name]; ok {
			language = openLanguage
		}
		if language == "" {
			continue
		}

		src, err := fs.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, nil, err
		}

		filename := ast.Filename(name)
		f, pDiags := parser.ParseFile(src, filename)
		diags[filename] = pDiags
		languages[filename] = language
		if f != nil {
			files[filename] = f
		}
	}

	return files, languages, diags, nil
}
