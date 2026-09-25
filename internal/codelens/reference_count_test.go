// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package codelens

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl-lang/reference"
)

func TestShowZeroReferences(t *testing.T) {
	addr := func(root, name string) lang.Address {
		return lang.Address{lang.RootStep{Name: root}, lang.AttrStep{Name: name}}
	}
	testCases := []struct {
		name     string
		targets  reference.Targets
		expected bool
	}{
		{"variable", reference.Targets{{Addr: addr("var", "stage"), ScopeId: "variable"}}, true},
		{"local", reference.Targets{{Addr: addr("local", "tags"), ScopeId: "local"}}, true},
		{"provider named local", reference.Targets{{Addr: lang.Address{lang.RootStep{Name: "local"}}, ScopeId: "provider"}}, false},
		{"provider alias", reference.Targets{{Addr: addr("local", "secondary"), ScopeId: "provider"}}, false},
		{"output", reference.Targets{{Addr: addr("output", "url"), ScopeId: "output"}}, false},
		{"resource", reference.Targets{{Addr: addr("random_pet", "name"), ScopeId: "resource"}}, false},
		{"module", reference.Targets{{Addr: addr("module", "app"), ScopeId: "module"}}, false},
		{"type-less and typed variable targets", reference.Targets{
			{Addr: addr("var", "stage")},
			{Addr: addr("var", "stage"), ScopeId: "variable"},
		}, true},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d-%s", i, tc.name), func(t *testing.T) {
			if got := showZeroReferences(tc.targets); got != tc.expected {
				t.Fatalf("expected %t, got %t", tc.expected, got)
			}
		})
	}
}
