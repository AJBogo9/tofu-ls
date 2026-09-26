// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refactor

import (
	"path/filepath"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/hashicorp/hcl/v2"
)

// DeclarationNameRangeInFile is DeclarationNameRange for a declaration
// whose file the caller already holds, parsed without errors, such as the
// module's index keeps it. The file is then not read and parsed again,
// which matters for requests sent on every cursor move.
func DeclarationNameRangeInFile(path lang.Path, target reference.Target, file *hcl.File) (hcl.Range, bool) {
	kind, ok := kindOfTarget(target)
	if !ok || target.RangePtr == nil || file == nil {
		return hcl.Range{}, false
	}
	fullPath := filepath.Join(path.Path, target.RangePtr.Filename)
	fc := &fileCache{
		src:   map[string][]byte{fullPath: file.Bytes},
		files: map[string]*hcl.File{fullPath: file},
	}
	sym, err := symbolFromTarget(fc, path, kind, target)
	if err != nil {
		return hcl.Range{}, false
	}
	return sym.NameRange, true
}
