// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
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
