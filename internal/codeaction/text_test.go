// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codeaction

import (
	"fmt"
	"testing"
)

func TestLineDiff(t *testing.T) {
	testCases := []struct {
		a, b string
		edit string
	}{
		{"a\nb\nc\n", "a\nx\nc\n", `f 2:1-3:1 "x\n"`},
		{"a\nb\n", "a\nb\nc\n", `f 3:1-3:1 "c\n"`},
		{"a\nb\nc\n", "c\n", `f 1:1-3:1 ""`},
		{"x", "y", `f 1:1-1:2 "y"`},
		{"a\r\nb\r\n", "a\r\nc\r\n", `f 2:1-3:1 "c\r\n"`},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			e, ok := lineDiff("/ws/f", []byte(tc.a), []byte(tc.b))
			if !ok {
				t.Fatal("expected an edit")
			}
			if got := describe([]Edit{e})[0]; got != tc.edit {
				t.Fatalf("expected %s, got %s", tc.edit, got)
			}
			if got := string(applyEdits([]byte(tc.a), []Edit{e})); got != tc.b {
				t.Fatalf("expected %q, got %q", tc.b, got)
			}
		})
	}
	if _, ok := lineDiff("/ws/f", []byte("same\n"), []byte("same\n")); ok {
		t.Fatal("expected no edit for equal texts")
	}
}

func TestAppendBlock(t *testing.T) {
	testCases := []struct {
		src, want string
	}{
		{"", "b {}\n"},
		{"a {}", "a {}\n\nb {}\n"},
		{"a {}\n", "a {}\n\nb {}\n"},
		{"a {}\n\n", "a {}\n\nb {}\n"},
		{"a {}\r\n", "a {}\r\n\r\nb {}\r\n"},
	}
	for i, tc := range testCases {
		e := appendBlock("/ws/f", []byte(tc.src), "b {}\n")
		if got := string(applyEdits([]byte(tc.src), []Edit{e})); got != tc.want {
			t.Errorf("%d: expected %q, got %q", i, tc.want, got)
		}
	}
}

func TestCRLF(t *testing.T) {
	fsys := files{"main.tf": "locals {\r\n  a      = 1\r\n  unused = 2\r\n}\r\n\r\nmodule \"c\" {\r\n  source = \"./child\"\r\n}\r\n"}

	diag := Diagnostic{Code: CodeUnusedLocal, Range: fsys.rangeOf(t, "main.tf", "unused", 1)}
	expectAction(t, fsys, QuickFixes(fsys.newEnv(), fsys.doc("main.tf"), diag), `Remove unused local "unused"`,
		[]string{`main.tf 2:1-4:1 "  a = 1\r\n"`},
		files{"main.tf": "locals {\r\n  a = 1\r\n}\r\n\r\nmodule \"c\" {\r\n  source = \"./child\"\r\n}\r\n"})

	diag = Diagnostic{Code: CodeMissingRequiredAttribute, Range: bodyRange(t, fsys, "main.tf", `module "c"`, 1)}
	expectAction(t, fsys, QuickFixes(schemaEnv(fsys), fsys.doc("main.tf"), diag), `Add required argument "name"`,
		[]string{`main.tf 8:1-8:1 "  name   = \"\"\r\n"`},
		files{"main.tf": "locals {\r\n  a      = 1\r\n  unused = 2\r\n}\r\n\r\nmodule \"c\" {\r\n  source = \"./child\"\r\n  name   = \"\"\r\n}\r\n"})
}
