// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package settings

import "testing"

func TestDecodeOptions_completion(t *testing.T) {
	testCases := []struct {
		name  string
		input map[string]interface{}
		want  bool
	}{
		{"on by default", map[string]interface{}{}, true},
		{
			"turned off",
			map[string]interface{}{
				"completion": map[string]interface{}{"addRequiredProviders": false},
			},
			false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := DecodeOptions(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.UnusedKeys) > 0 {
				t.Fatalf("unexpected unused keys %q", out.UnusedKeys)
			}
			if got := out.Options.Completion.AddRequiredProviders; got != tc.want {
				t.Fatalf("addRequiredProviders: got %v, want %v", got, tc.want)
			}
		})
	}
}
