// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package filepaths

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func parse(t *testing.T, src string) *hcl.File {
	t.Helper()
	f, diags := hclsyntax.ParseConfig([]byte(src), "main.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	return f
}

// posOf returns the position of the | marker and the source without it.
func posOf(src string) (string, hcl.Pos) {
	offset := strings.Index(src, "|")
	return strings.Replace(src, "|", "", 1), hcl.Pos{
		Line:   strings.Count(src[:offset], "\n") + 1,
		Column: offset - strings.LastIndex(src[:offset], "\n"),
		Byte:   offset,
	}
}

func TestCalls(t *testing.T) {
	f := parse(t, `locals {
  a = templatefile("${path.module}/templates/a.tftpl", { x = file("b.txt") })
  b = [for f in fileset(path.module, "*.json") : jsondecode(file(f))]
  c = core::filebase64("c.bin")
  d = upper("not a file")
  e = fileexists("maybe.txt") ? 1 : 0
}
`)
	got := make([]string, 0)
	for _, c := range Calls(f.Body) {
		rng := c.Path.Range()
		got = append(got, fmt.Sprintf("%s %d %s", c.Function, c.Kind, string(f.Bytes[rng.Start.Byte:rng.End.Byte])))
	}
	want := []string{
		`templatefile 0 "${path.module}/templates/a.tftpl"`,
		`file 0 "b.txt"`,
		`fileset 2 path.module`,
		`file 0 f`,
		`filebase64 0 "c.bin"`,
		`fileexists 1 "maybe.txt"`,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected calls: %s", diff)
	}
}

func TestStaticPath(t *testing.T) {
	testCases := []struct {
		expr         string
		wantPath     string
		wantAnchored bool
		wantOK       bool
	}{
		{`"templates/a.tftpl"`, "templates/a.tftpl", false, true},
		{`"${path.module}/templates/a.tftpl"`, "./templates/a.tftpl", true, true},
		{`"${path.root}/files/x"`, "./files/x", false, true},
		{`path.module`, ".", true, true},
		{`"/etc/hosts"`, "/etc/hosts", true, true},
		{`"${var.dir}/a"`, "", false, false},
		{`"${local.dir}/a"`, "", false, false},
		{`"${path.cwd}/a"`, "", false, false},
		{`format("%s/a", path.module)`, "", false, false},
		{`""`, "", false, false},
	}
	for _, tc := range testCases {
		t.Run(tc.expr, func(t *testing.T) {
			expr, diags := hclsyntax.ParseExpression([]byte(tc.expr), "x.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			path, anchored, ok := StaticPath(expr)
			if path != tc.wantPath || anchored != tc.wantAnchored || ok != tc.wantOK {
				t.Fatalf("got %q %v %v, want %q %v %v", path, anchored, ok, tc.wantPath, tc.wantAnchored, tc.wantOK)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	mod := filepath.FromSlash("/w/modules/app")
	root := filepath.FromSlash("/w")
	testCases := []struct {
		path     string
		anchored bool
		rootDir  string
		want     string
	}{
		{"./templates/a.tftpl", true, root, "/w/modules/app/templates/a.tftpl"},
		{"files/x", false, root, "/w/files/x"},
		{"files/x", false, "", "/w/modules/app/files/x"},
		{"/etc/hosts", true, root, "/etc/hosts"},
	}
	for _, tc := range testCases {
		if got := Resolve(tc.path, tc.anchored, mod, tc.rootDir); got != filepath.FromSlash(tc.want) {
			t.Fatalf("%s: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestLiteralAtPos(t *testing.T) {
	testCases := []struct {
		name         string
		src          string
		wantTyped    string
		wantAnchored bool
		wantOK       bool
	}{
		{"plain path", `x = file("templates/co|nfig.json")`, "templates/co", false, true},
		{"after path.module", `x = templatefile("${path.module}/templates/|", {})`, "/templates/", true, true},
		{"right after path.module", `x = templatefile("${path.module}|", {})`, "", true, true},
		{"empty string", `x = file("|")`, "", false, true},
		{"inside the interpolation", `x = file("${path.mo|dule}/a")`, "", false, false},
		{"another argument", `x = templatefile("a", { b = "|" })`, "", false, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			src, pos := posOf(tc.src)
			f := parse(t, src+"\n")
			call, ok := CallAtPos(f.Body, pos)
			if !ok {
				if tc.wantOK {
					t.Fatal("no call at the position")
				}
				return
			}
			typed, _, anchored, ok := LiteralAtPos(call.Path, f.Bytes, pos)
			if typed != tc.wantTyped || anchored != tc.wantAnchored || ok != tc.wantOK {
				t.Fatalf("got %q %v %v, want %q %v %v", typed, anchored, ok, tc.wantTyped, tc.wantAnchored, tc.wantOK)
			}
		})
	}
}

func TestTextRange(t *testing.T) {
	f := parse(t, `x = file("${path.module}/a.txt")
y = file(local.p)
`)
	got := make([]string, 0)
	for _, c := range Calls(f.Body) {
		rng := TextRange(c.Path, f.Bytes)
		got = append(got, string(f.Bytes[rng.Start.Byte:rng.End.Byte]))
	}
	if diff := cmp.Diff([]string{"${path.module}/a.txt", "local.p"}, got); diff != "" {
		t.Fatalf("unexpected ranges: %s", diff)
	}
}
