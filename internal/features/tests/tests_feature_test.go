// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tests

import (
	"context"
	"testing"

	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

// The walker announces a directory with test files (Discover), and the
// feature queues the jobs which parse them. Queued on the feature's own
// time, they could miss a wait for the jobs queued so far, such as a
// request's (a quick fix left the test file's variables out). Discover
// returns once the jobs are queued.
func TestTestsFeature_discoverQueuesJobsBeforeReturning(t *testing.T) {
	for i := 0; i < 50; i++ {
		ss, err := globalState.NewStateStore()
		if err != nil {
			t.Fatal(err)
		}
		bus := eventbus.NewEventBus()
		f, err := NewTestsFeature(bus, ss, filesystem.NewFilesystem(ss.DocumentStore), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		f.Start(ctx)

		dir := document.DirHandleFromPath(t.TempDir())
		bus.Discover(eventbus.DiscoverEvent{Path: dir.Path(), Files: []string{"main.tf", "main.tftest.hcl"}})
		ids, err := ss.JobStore.ListIncompleteJobsForDir(dir)
		cancel()
		f.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			t.Fatalf("run %d: no jobs queued for the test file when Discover returned", i)
		}
	}
}
