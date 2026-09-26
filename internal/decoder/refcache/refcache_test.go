// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package refcache

import (
	"testing"

	"github.com/hashicorp/hcl-lang/decoder"
)

type record struct{ version int }

func TestCache_Get(t *testing.T) {
	builds := 0
	build := func(r *record) *decoder.PathContext {
		builds++
		return &decoder.PathContext{}
	}

	testCases := []struct {
		name  string
		cache *Cache[record]
	}{
		{"cache", New[record]()},
		{"nil cache builds every time", nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			builds = 0
			v1 := &record{version: 1}
			first := tc.cache.Get("/m", v1, build)
			again := tc.cache.Get("/m", v1, build)
			wantBuilds := 1
			if tc.cache == nil {
				wantBuilds = 2
			} else if first != again {
				t.Fatal("the same record built a second context")
			}
			if builds != wantBuilds {
				t.Fatalf("expected %d builds, got %d", wantBuilds, builds)
			}

			// a changed module is a new record
			v2 := &record{version: 2}
			if tc.cache.Get("/m", v2, build) == first {
				t.Fatal("a new record got the old context")
			}
			// another path has its own entry
			tc.cache.Get("/other", v2, build)
			builds = 0
			tc.cache.Get("/m", v2, build)
			if tc.cache != nil && builds != 0 {
				t.Fatal("the context of the new record was not kept")
			}

			tc.cache.Forget("/m")
			builds = 0
			tc.cache.Get("/m", v2, build)
			if builds != 1 {
				t.Fatalf("expected a build after Forget, got %d", builds)
			}
		})
	}
}
