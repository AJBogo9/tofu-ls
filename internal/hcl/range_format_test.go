// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// messy and messyFormatted are the input and the output of tofu fmt.
const messy = `resource "a" "b" {
  x=1
    yy = 2
}

resource   "c" "d"   {
foo="bar"
  list = [1,2,
  3]
}

# comment
variable "v" {
default=1
    type = string
}
`

const messyFormatted = `resource "a" "b" {
  x  = 1
  yy = 2
}

resource "c" "d" {
  foo = "bar"
  list = [1, 2,
  3]
}

# comment
variable "v" {
  default = 1
  type    = string
}
`

func TestFormatLines(t *testing.T) {
	testCases := []struct {
		name               string
		startLine, endLine int
		want               string
	}{
		{
			"one line inside the second block formats that block only",
			6, 6,
			`resource "a" "b" {
  x=1
    yy = 2
}

resource "c" "d" {
  foo = "bar"
  list = [1, 2,
  3]
}

# comment
variable "v" {
default=1
    type = string
}
`,
		},
		{
			"a selection over two blocks",
			2, 5,
			`resource "a" "b" {
  x  = 1
  yy = 2
}

resource "c" "d" {
  foo = "bar"
  list = [1, 2,
  3]
}

# comment
variable "v" {
default=1
    type = string
}
`,
		},
		{
			"blank line and comment between blocks: nothing",
			10, 11,
			messy,
		},
		{
			"the whole file",
			0, 16,
			messyFormatted,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FormatLines("main.tf", []byte(messy), []byte(messyFormatted), tc.startLine, tc.endLine)
			if !ok {
				t.Fatal("expected the items to correspond")
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Fatalf("unexpected result: %s", diff)
			}
		})
	}
}

func TestFormatLines_mismatch(t *testing.T) {
	if _, ok := FormatLines("main.tf", []byte(messy), []byte("locals {}\n"), 0, 1); ok {
		t.Fatal("expected no result when the items differ")
	}
	if _, ok := FormatLines("main.tf", []byte("resource \"a\" {\n"), []byte("resource \"a\" {\n"), 0, 1); ok {
		t.Fatal("expected no result for a syntax error")
	}
}

func TestFormatLines_tfvars(t *testing.T) {
	before := "a=1\nlong_name = 2\n\nc   = 3\n"
	after := "a         = 1\nlong_name = 2\n\nc = 3\n"
	got, ok := FormatLines("x.tfvars", []byte(before), []byte(after), 0, 0)
	if !ok {
		t.Fatal("expected a result")
	}
	want := "a         = 1\nlong_name = 2\n\nc   = 3\n"
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Fatalf("unexpected result: %s", diff)
	}
}
