// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	tfjson "github.com/hashicorp/terraform-json"
	tfmod "github.com/opentofu/opentofu-schema/module"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/features/rootmodules"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/registry"
	"github.com/opentofu/tofu-ls/internal/settings"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	"github.com/opentofu/tofu-ls/internal/tofu/exec"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
	"github.com/stretchr/testify/mock"
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

// TestProviderSchemasChange_revalidates checks that a module is validated
// again once the provider schemas of its root module are obtained after
// its first validation, as they often are (tofu providers schema takes
// seconds): an unknown resource type is reported without another edit.
func TestProviderSchemasChange_revalidates(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.tf": "resource \"random_pet\" \"ok\" {}\nresource \"random_nope\" \"x\" {}\n",
		".terraform.lock.hcl": `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.6.0"
}
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	schemas := &tfjson.ProviderSchemas{
		FormatVersion: "1.0",
		Schemas: map[string]*tfjson.ProviderSchema{
			"registry.opentofu.org/hashicorp/random": {
				ConfigSchema: &tfjson.Schema{Block: &tfjson.SchemaBlock{}},
				ResourceSchemas: map[string]*tfjson.Schema{
					"random_pet": {Block: &tfjson.SchemaBlock{}},
				},
			},
		},
	}
	execFactory := exec.NewMockExecutor(&exec.TofuMockCalls{
		PerWorkDir: map[string][]*mock.Call{
			dir: {
				{
					Method:          "Version",
					Repeatability:   1,
					Arguments:       []interface{}{mock.AnythingOfType("")},
					ReturnArguments: []interface{}{version.Must(version.NewVersion("1.12.6")), nil, nil},
				},
				{
					Method:          "ProviderSchemas",
					Repeatability:   1,
					Arguments:       []interface{}{mock.AnythingOfType("")},
					ReturnArguments: []interface{}{schemas, nil},
				},
			},
		},
	})

	ss, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.NewEventBus()
	fs := filesystem.NewFilesystem(ss.DocumentStore)
	roots, err := rootmodules.NewRootModulesFeature(bus, ss, fs, execFactory)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewModulesFeature(bus, ss, fs, roots, registry.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	roots.Start(ctx)
	f.Start(ctx)
	// the walker discovers the root module by its lock file
	if err := roots.Store.Add(dir); err != nil {
		t.Fatal(err)
	}

	opts := settings.ValidationOptions{EnableEnhancedValidation: true, UnknownResourceTypes: true}
	dh := document.DirHandleFromPath(dir)
	reqCtx := lsctx.WithValidationOptions(lsctx.WithDocumentContext(ctx, lsctx.Document{
		Method:     "textDocument/didOpen",
		LanguageID: "opentofu",
		URI:        dh.URI + "/main.tf",
	}), &opts)
	bus.DidOpen(eventbus.DidOpenEvent{Context: reqCtx, Dir: dh, LanguageID: "opentofu"})

	jobCtx := exec.WithExecutorOpts(ctx, &exec.ExecutorOpts{ExecPath: "tofu"})
	unknownTypes := func() []string {
		mod, err := f.Store.ModuleRecordByPath(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, d := range mod.ModuleDiagnostics[globalAst.SemanticValidationSource]["main.tf"] {
			got = append(got, fmt.Sprintf("%d:%d %s", d.Subject.Start.Line, d.Subject.Start.Column, d.Summary))
		}
		return got
	}

	// every job but the one obtaining the schemas, which is held back
	held, reruns := runJobsHolding(t, ss, jobCtx, op.OpTypeObtainSchema.String())
	if len(held) != 1 || reruns != 0 {
		t.Fatalf("expected one job obtaining the schemas and no validation again, got %d and %d", len(held), reruns)
	}
	if got := unknownTypes(); len(got) != 0 {
		t.Fatalf("expected no diagnostics without the schemas, got %q", got)
	}

	held[0].run(t, ss)
	_, reruns = runJobsHolding(t, ss, jobCtx, "")
	if got := strings.Join(unknownTypes(), "\n"); got != "2:10 Invalid resource type" || reruns != 1 {
		t.Fatalf("expected the unknown resource type from one validation again once the schemas are in, got %q from %d", got, reruns)
	}

	// opening another file finds the schemas obtained: no validation again
	bus.DidOpen(eventbus.DidOpenEvent{Context: reqCtx, Dir: dh, LanguageID: "opentofu"})
	if _, reruns = runJobsHolding(t, ss, jobCtx, ""); reruns != 0 {
		t.Fatalf("expected no validation again, got %d", reruns)
	}
}

type heldJob struct {
	ctx context.Context
	id  job.ID
	job job.Job
}

// run runs the job and its deferred function and finishes it.
func (h heldJob) run(t *testing.T, ss *globalState.StateStore) {
	jobErr := h.job.Func(h.ctx)
	var deferred job.IDs
	if h.job.Defer != nil {
		deferred, _ = h.job.Defer(h.ctx, jobErr)
	}
	if err := ss.JobStore.FinishJob(h.id, jobErr, deferred...); err != nil {
		t.Fatal(err)
	}
}

// runJobsHolding runs the queued jobs as the scheduler would until none
// is left, except the jobs of type hold, which it returns unfinished. It
// also returns how many semantic validations ran again (ignoring state).
func runJobsHolding(t *testing.T, ss *globalState.StateStore, base context.Context, hold string) ([]heldJob, int) {
	var held []heldJob
	reruns := 0
	for {
		ctx, cancel := context.WithTimeout(base, 300*time.Millisecond)
		jobCtx, id, j, err := ss.JobStore.AwaitNextJob(ctx, job.LowPriority)
		cancel()
		if err != nil {
			return held, reruns
		}
		// the job runs with the base context, not the waiting one
		jobCtx = job.WithIgnoreState(lsctx.WithDocumentContext(base, lsctx.DocumentContext(jobCtx)), j.IgnoreState)
		h := heldJob{ctx: jobCtx, id: id, job: j}
		if j.Type == hold {
			held = append(held, h)
			continue
		}
		if j.Type == op.OpTypeSemanticValidation.String() && j.IgnoreState {
			reruns++
		}
		h.run(t, ss)
	}
}
