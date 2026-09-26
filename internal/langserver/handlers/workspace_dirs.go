// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"path/filepath"
	"strings"
	"sync"
)

// workspaceDirs tracks the workspace folders the client opened, so that
// features which edit files (rename) can refuse to change modules outside
// them.
type workspaceDirs struct {
	mu   sync.Mutex
	dirs []string
}

func (w *workspaceDirs) add(dir string) {
	if dir == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirs = append(w.dirs, filepath.Clean(dir))
}

func (w *workspaceDirs) remove(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dir = filepath.Clean(dir)
	kept := w.dirs[:0]
	for _, d := range w.dirs {
		if d != dir {
			kept = append(kept, d)
		}
	}
	w.dirs = kept
}

// contains reports whether dir is inside a workspace folder. Without any
// known folder (single file mode) every directory is.
func (w *workspaceDirs) contains(dir string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.dirs) == 0 {
		return true
	}
	dir = filepath.Clean(dir)
	for _, d := range w.dirs {
		rel, err := filepath.Rel(d, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
