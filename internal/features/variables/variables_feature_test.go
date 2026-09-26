// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package variables

import (
	"context"
	"testing"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

// The walker announces a directory (Discover) before a document of it
// opens. Handled on the feature's own time, the announcement could come
// after the didOpen, which then found no record of the directory and never
// parsed its tfvars (a rename left terraform.tfvars out). Discover returns
// once the feature has recorded the directory.
func TestVariablesFeature_discoverBeforeDidOpen(t *testing.T) {
	for i := 0; i < 50; i++ {
		ss, err := globalState.NewStateStore()
		if err != nil {
			t.Fatal(err)
		}
		bus := eventbus.NewEventBus()
		f, err := NewVariablesFeature(bus, ss, filesystem.NewFilesystem(ss.DocumentStore), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		f.Start(ctx)

		dir := document.DirHandleFromPath(t.TempDir())
		bus.Discover(eventbus.DiscoverEvent{Path: dir.Path(), Files: []string{"main.tf", "terraform.tfvars"}})
		bus.DidOpen(eventbus.DidOpenEvent{
			Context:    lsctx.WithDocumentContext(ctx, lsctx.Document{}),
			Dir:        dir,
			LanguageID: "opentofu",
		})
		ids, err := ss.JobStore.ListIncompleteJobsForDir(dir)
		cancel()
		f.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			t.Fatalf("run %d: opening main.tf queued no jobs for the tfvars", i)
		}
	}
}
