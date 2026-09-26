// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
)

// root is the directory the test files live in.
const root = "/ws"

// files maps paths relative to root to their content.
type files map[string]string

func (fsys files) mapFS() fstest.MapFS {
	m := fstest.MapFS{}
	for name, content := range fsys {
		m[strings.TrimPrefix(filepath.Join(root, name), "/")] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// newEnv reads the files. Other functions are set by the tests.
func (fsys files) newEnv() Env {
	m := fsys.mapFS()
	return Env{
		ReadFile: func(path string) ([]byte, error) {
			return fs.ReadFile(m, strings.TrimPrefix(path, "/"))
		},
		ReadDir: func(dir string) ([]fs.DirEntry, error) {
			return fs.ReadDir(m, strings.TrimPrefix(dir, "/"))
		},
	}
}

func (fsys files) doc(name string) Document {
	return Document{
		Path: filepath.Join(root, name),
		Text: []byte(fsys[name]),
		Vars: strings.HasSuffix(name, ".tfvars"),
	}
}

// rangeOf is the range of the nth (1-based) occurrence of needle in a
// file.
func (fsys files) rangeOf(t *testing.T, name, needle string, nth int) hcl.Range {
	t.Helper()
	src := []byte(fsys[name])
	idx := -1
	for i := 0; i < nth; i++ {
		next := strings.Index(string(src[idx+1:]), needle)
		if next < 0 {
			t.Fatalf("%q (occurrence %d) not in %s", needle, nth, name)
		}
		idx += next + 1
	}
	return rangeAt(filepath.Join(root, name), src, idx, idx+len(needle))
}

// describe renders edits as "file line:col-line:col replacement" lines,
// with columns counted in bytes, for exact comparison.
func describe(edits []Edit) []string {
	out := make([]string, 0, len(edits))
	for _, e := range edits {
		rel := strings.TrimPrefix(e.File, root+"/")
		out = append(out, fmt.Sprintf("%s %d:%d-%d:%d %q", rel,
			e.Range.Start.Line, e.Range.Start.Column, e.Range.End.Line, e.Range.End.Column, e.NewText))
	}
	sort.Strings(out)
	return out
}

// apply returns the files an action's edits change, with their new
// content.
func apply(t *testing.T, fsys files, a Action) files {
	t.Helper()
	byFile := map[string][]Edit{}
	for _, e := range a.Edits {
		byFile[e.File] = append(byFile[e.File], e)
	}
	out := files{}
	for file, edits := range byFile {
		rel := strings.TrimPrefix(file, root+"/")
		src := []byte(fsys[rel])
		for _, e := range edits {
			if e.Range.End.Byte > len(src) || e.Range.Start.Byte > e.Range.End.Byte {
				t.Fatalf("edit out of range: %v", describe([]Edit{e}))
			}
			// the lines must agree with the bytes
			if want := posAt(src, e.Range.Start.Byte); want != e.Range.Start {
				t.Fatalf("start %#v does not match its byte offset (%#v)", e.Range.Start, want)
			}
			if want := posAt(src, e.Range.End.Byte); want != e.Range.End {
				t.Fatalf("end %#v does not match its byte offset (%#v)", e.Range.End, want)
			}
		}
		for i := range edits {
			for j := range edits {
				if i != j && edits[i].Range.Start.Byte < edits[j].Range.End.Byte && edits[j].Range.Start.Byte < edits[i].Range.End.Byte {
					t.Fatalf("overlapping edits: %v", describe(edits))
				}
			}
		}
		out[rel] = string(applyEdits(src, edits))
	}
	return out
}

// expectAction checks the only action's title, its exact edits and the
// content of every file it changes.
func expectAction(t *testing.T, fsys files, actions []Action, title string, edits []string, want files) Action {
	t.Helper()
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d: %#v", len(actions), actions)
	}
	a := actions[0]
	if a.Title != title {
		t.Errorf("title: expected %q, got %q", title, a.Title)
	}
	got := describe(a.Edits)
	sort.Strings(edits)
	if strings.Join(got, "\n") != strings.Join(edits, "\n") {
		t.Errorf("edits:\nexpected\n%s\ngot\n%s", strings.Join(edits, "\n"), strings.Join(got, "\n"))
	}
	result := apply(t, fsys, a)
	if len(result) != len(want) {
		t.Errorf("expected changes to %d files, got %d: %v", len(want), len(result), result)
	}
	for name, content := range want {
		if result[name] != content {
			t.Errorf("%s:\nexpected\n%s\ngot\n%s", name, content, result[name])
		}
	}
	return a
}
