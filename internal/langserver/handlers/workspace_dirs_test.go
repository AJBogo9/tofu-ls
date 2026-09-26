// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceDirs_contains(t *testing.T) {
	var w workspaceDirs
	if !w.contains("/any/dir") {
		t.Fatal("without folders every directory is in the workspace")
	}
	w.add("/mono/envs/prod")
	w.add("/other")
	testCases := []struct {
		dir  string
		want bool
	}{
		{"/mono/envs/prod", true},
		{"/mono/envs/prod/modules/app", true},
		{"/mono/modules/app", false},
		{"/mono/envs/production", false},
		{"/other/x", true},
	}
	for _, tc := range testCases {
		t.Run(tc.dir, func(t *testing.T) {
			if got := w.contains(tc.dir); got != tc.want {
				t.Fatalf("contains(%q) = %t, want %t", tc.dir, got, tc.want)
			}
		})
	}
	w.remove("/other")
	if w.contains("/other/x") {
		t.Fatal("a removed folder is still in the workspace")
	}
}

func TestWorkspaceDirs_rel(t *testing.T) {
	var w workspaceDirs
	w.add("/mono")
	w.add("/mono/envs/prod")
	testCases := []struct {
		path string
		want string
	}{
		// the file of a child module, not only its base name
		{"/mono/modules/app/main.tf", filepath.Join("modules", "app", "main.tf")},
		// the innermost folder
		{"/mono/envs/prod/main.tf", "main.tf"},
		{"/elsewhere/main.tf", ""},
	}
	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			if got := w.rel(tc.path); got != tc.want {
				t.Fatalf("rel(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
