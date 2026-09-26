// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tests

import (
	"context"
	"io"
	"log"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/tests/decoder"
	"github.com/opentofu/tofu-ls/internal/features/tests/jobs"
	"github.com/opentofu/tofu-ls/internal/features/tests/state"
	"github.com/opentofu/tofu-ls/internal/job"
	"github.com/opentofu/tofu-ls/internal/langserver/diagnostics"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

// TestsFeature groups everything related to the test files
// (*.tftest.hcl, *.tofutest.hcl) and mock data files (*.tfmock.hcl)
// of tofu test. Its internal state keeps track of the directories which
// hold them.
type TestsFeature struct {
	store    *state.TestStore
	eventbus *eventbus.EventBus
	stopFunc context.CancelFunc
	logger   *log.Logger

	moduleFeature ModuleFeature
	stateStore    *globalState.StateStore
	fs            jobs.ReadOnlyFS
}

// ModuleFeature reads the modules which the runs of test files run, and
// indexes them.
type ModuleFeature interface {
	fdecoder.ModuleReader
	DecodeLocalModule(ctx context.Context, modPath string) (job.IDs, error)
}

func NewTestsFeature(eventbus *eventbus.EventBus, stateStore *globalState.StateStore, fs jobs.ReadOnlyFS, moduleFeature ModuleFeature) (*TestsFeature, error) {
	store, err := state.NewTestStore(stateStore.ChangeStore)
	if err != nil {
		return nil, err
	}
	discardLogger := log.New(io.Discard, "", 0)

	return &TestsFeature{
		store:         store,
		eventbus:      eventbus,
		stopFunc:      func() {},
		logger:        discardLogger,
		moduleFeature: moduleFeature,
		stateStore:    stateStore,
		fs:            fs,
	}, nil
}

func (f *TestsFeature) SetLogger(logger *log.Logger) {
	f.logger = logger
	f.store.SetLogger(logger)
}

// Start starts the features separate goroutine.
// It listens to various events from the EventBus and performs corresponding actions.
func (f *TestsFeature) Start(ctx context.Context) {
	ctx, cancelFunc := context.WithCancel(ctx)
	f.stopFunc = cancelFunc

	// the walker waits until a directory's test files are recorded and
	// their jobs queued, so that waiting for the queued jobs waits for them
	discoverDone := make(chan struct{}, 10)
	discover := f.eventbus.OnDiscover("feature.tests", discoverDone)

	didOpenDone := make(chan struct{}, 10)
	didOpen := f.eventbus.OnDidOpen("feature.tests", didOpenDone)

	didChangeDone := make(chan struct{}, 10)
	didChange := f.eventbus.OnDidChange("feature.tests", didChangeDone)

	didChangeWatchedDone := make(chan struct{}, 10)
	didChangeWatched := f.eventbus.OnDidChangeWatched("feature.tests", didChangeWatchedDone)

	go func() {
		for {
			select {
			case discover := <-discover:
				// the walker sends no context, and jobs need a document one
				f.discover(lsctx.WithDocumentContext(ctx, lsctx.Document{}), discover.Path, discover.Files)
				discoverDone <- struct{}{}
			case didOpen := <-didOpen:
				f.didOpen(didOpen.Context, didOpen.Dir, didOpen.LanguageID)
				didOpenDone <- struct{}{}
			case didChange := <-didChange:
				f.didChange(didChange.Context, didChange.Dir)
				didChangeDone <- struct{}{}
			case didChangeWatched := <-didChangeWatched:
				f.didChangeWatched(didChangeWatched.Context, didChangeWatched.RawPath, didChangeWatched.ChangeType, didChangeWatched.IsDir)
				didChangeWatchedDone <- struct{}{}

			case <-ctx.Done():
				return
			}
		}
	}()
}

func (f *TestsFeature) Stop() {
	f.stopFunc()
	f.logger.Print("stopped tests feature")
}

func (f *TestsFeature) pathReader() *fdecoder.PathReader {
	return &fdecoder.PathReader{
		StateReader:  f.store,
		ModuleReader: f.moduleFeature,
	}
}

func (f *TestsFeature) PathContext(path lang.Path) (*decoder.PathContext, error) {
	return f.pathReader().PathContext(path)
}

// ReferencePathContext returns the reference origins and targets and the
// files of the test files, without their schema (see
// decoder.ReferencePathReader).
func (f *TestsFeature) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	return f.pathReader().ReferencePathContext(path)
}

func (f *TestsFeature) Paths(ctx context.Context) []lang.Path {
	return f.pathReader().Paths(ctx)
}

func (f *TestsFeature) Diagnostics(path string) diagnostics.Diagnostics {
	diags := diagnostics.NewDiagnostics()

	record, err := f.store.TestRecordByPath(path)
	if err != nil {
		return diags
	}

	for source, dm := range record.Diagnostics {
		diags.Append(source, dm.AsMap())
	}

	return diags
}
