// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package modules

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	tfmod "github.com/opentofu/opentofu-schema/module"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/registry"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

type noRoots struct{}

func (noRoots) InstalledModuleCalls(string) (map[string]tfmod.InstalledModuleCall, error) {
	return nil, nil
}
func (noRoots) TofuVersion(string) *version.Version               { return nil }
func (noRoots) InstalledModulePath(string, string) (string, bool) { return "", false }

// DecodeLocalModule, which the test files use to index the modules their
// runs run, must not run ahead of the jobs queued for the module, such as
// the provider schemas of an open file: the references would miss them.
func TestDecodeLocalModule_afterQueuedJobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("variable \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewModulesFeature(eventbus.NewEventBus(), ss, filesystem.NewFilesystem(ss.DocumentStore), noRoots{}, registry.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})
	dh := document.DirHandleFromPath(dir)

	queued, err := ss.JobStore.EnqueueJob(ctx, job.Job{
		Dir:  dh,
		Func: func(context.Context) error { return nil },
		Type: "queued before",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := f.DecodeLocalModule(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no jobs scheduled")
	}

	next := func() job.ID {
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, id, _, err := ss.JobStore.AwaitNextJob(ctx, job.LowPriority)
		if err != nil {
			return ""
		}
		return id
	}
	if id := next(); id != queued {
		t.Fatalf("expected the queued job %q first, got %q", queued, id)
	}
	if id := next(); id != "" {
		t.Fatalf("job %q ran before the queued job finished", id)
	}
	if err := ss.JobStore.FinishJob(queued, nil); err != nil {
		t.Fatal(err)
	}
	if id := next(); id != ids[0] {
		t.Fatalf("expected the parsing job %q, got %q", ids[0], id)
	}
}

// runQueuedJobs runs the jobs of the store until none is left, as the
// scheduler would.
func runQueuedJobs(t *testing.T, ss *globalState.StateStore) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		jobCtx, id, j, err := ss.JobStore.AwaitNextJob(ctx, job.LowPriority)
		if err != nil {
			cancel()
			return
		}
		jobErr := j.Func(jobCtx)
		var deferred job.IDs
		if j.Defer != nil {
			deferred, _ = j.Defer(jobCtx, jobErr)
		}
		cancel()
		if err := ss.JobStore.FinishJob(id, jobErr, deferred...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecodeLocalModule_indexedAlready(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("variable \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewModulesFeature(eventbus.NewEventBus(), ss, filesystem.NewFilesystem(ss.DocumentStore), noRoots{}, registry.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})

	ids, err := f.DecodeLocalModule(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no jobs scheduled for a module that is not indexed")
	}
	runQueuedJobs(t, ss)
	meta, err := f.LocalModuleMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta.Variables["x"]; !ok {
		t.Fatalf("variable x not indexed: %#v", meta.Variables)
	}

	// indexed now: nothing more to do
	ids, err = f.DecodeLocalModule(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no jobs, got %v", ids)
	}
}
