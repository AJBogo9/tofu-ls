// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package walker

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	"github.com/opentofu/tofu-ls/internal/state"
)

func TestWalker_basic(t *testing.T) {
	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	fs := filesystem.NewFilesystem(ss.DocumentStore)
	pa := state.NewPathAwaiter(ss.WalkerPaths, false)
	bus := eventbus.NewEventBus()

	w := NewWalker(fs, pa, bus)
	w.Collector = NewWalkerCollector()
	w.SetLogger(testLogger())

	root, err := filepath.Abs(filepath.Join("testdata", "uninitialized-root"))
	if err != nil {
		t.Fatal(err)
	}
	dir := document.DirHandleFromPath(root)

	ctx := context.Background()
	err = ss.WalkerPaths.EnqueueDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx = lsctx.WithDocumentContext(ctx, lsctx.Document{})
	err = w.StartWalking(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = ss.WalkerPaths.WaitForDirs(ctx, []document.DirHandle{dir})
	if err != nil {
		t.Fatal(err)
	}
	err = ss.JobStore.WaitForJobs(ctx, w.Collector.JobIds()...)
	if err != nil {
		t.Fatal(err)
	}
	err = w.Collector.ErrorOrNil()
	if err != nil {
		t.Fatal(err)
	}
}

func testLogger() *log.Logger {
	if testing.Verbose() {
		return log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)
	}

	return log.New(io.Discard, "", 0)
}

func TestWalker_isSkippableDir(t *testing.T) {
	w := NewWalker(nil, nil, nil)

	testCases := []struct {
		name string
		skip bool
	}{
		{".git", true},
		{".terragrunt-cache", true},
		// JavaScript tooling next to a configuration, e.g. a CDK or an
		// editor extension checkout, holds tens of thousands of dirs
		{"node_modules", true},
		{".vscode-test", true},
		{"modules", false},
		{".terraform", false},
		{"node_modules_backup", false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.isSkippableDir(tc.name); got != tc.skip {
				t.Fatalf("expected skip=%t for %q, got %t", tc.skip, tc.name, got)
			}
		})
	}
}
