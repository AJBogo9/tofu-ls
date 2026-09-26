// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package terragrunt

import (
	"context"
	"io"
	"log"
	"path/filepath"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/eventbus"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/terragrunt/decoder"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/jobs"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/langserver/diagnostics"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
)

// TerragruntFeature groups everything related to Terragrunt files:
// terragrunt.hcl and root.hcl (language terragrunt) and
// terragrunt.stack.hcl (terragrunt-stack), and any .hcl file the editor
// opens with one of these languages. Only the directories of open files
// are indexed: nothing here runs workspace-wide.
type TerragruntFeature struct {
	store    *state.TerragruntStore
	eventbus *eventbus.EventBus
	stopFunc context.CancelFunc
	logger   *log.Logger

	stateStore *globalState.StateStore
	fs         jobs.ReadOnlyFS
	includes   *fdecoder.IncludeCache
}

func NewTerragruntFeature(eventbus *eventbus.EventBus, stateStore *globalState.StateStore, fs jobs.ReadOnlyFS) (*TerragruntFeature, error) {
	store, err := state.NewTerragruntStore(stateStore.ChangeStore)
	if err != nil {
		return nil, err
	}
	discardLogger := log.New(io.Discard, "", 0)

	return &TerragruntFeature{
		store:      store,
		eventbus:   eventbus,
		stopFunc:   func() {},
		logger:     discardLogger,
		stateStore: stateStore,
		fs:         fs,
		includes:   fdecoder.NewIncludeCache(),
	}, nil
}

func (f *TerragruntFeature) SetLogger(logger *log.Logger) {
	f.logger = logger
	f.store.SetLogger(logger)
}

// Start starts the features separate goroutine.
// It listens to various events from the EventBus and performs corresponding actions.
func (f *TerragruntFeature) Start(ctx context.Context) {
	ctx, cancelFunc := context.WithCancel(ctx)
	f.stopFunc = cancelFunc

	didOpenDone := make(chan struct{}, 10)
	didOpen := f.eventbus.OnDidOpen("feature.terragrunt", didOpenDone)

	didChangeDone := make(chan struct{}, 10)
	didChange := f.eventbus.OnDidChange("feature.terragrunt", didChangeDone)

	didChangeWatchedDone := make(chan struct{}, 10)
	didChangeWatched := f.eventbus.OnDidChangeWatched("feature.terragrunt", didChangeWatchedDone)

	go func() {
		for {
			select {
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

func (f *TerragruntFeature) Stop() {
	f.stopFunc()
	f.logger.Print("stopped terragrunt feature")
}

func (f *TerragruntFeature) pathReader() *fdecoder.PathReader {
	return &fdecoder.PathReader{
		StateReader: f.store,
		FS:          f.fs,
		Includes:    f.includes,
	}
}

func (f *TerragruntFeature) PathContext(path lang.Path) (*decoder.PathContext, error) {
	return f.pathReader().PathContext(path)
}

// ReferencePathContext returns the reference origins and targets and the
// files of the Terragrunt files, without their schema (see
// decoder.ReferencePathReader).
func (f *TerragruntFeature) ReferencePathContext(path lang.Path) (*decoder.PathContext, error) {
	return f.pathReader().ReferencePathContext(path)
}

func (f *TerragruntFeature) Paths(ctx context.Context) []lang.Path {
	return f.pathReader().Paths(ctx)
}

// Diagnostics returns the syntax errors and the schema warnings of the
// Terragrunt files of a directory, all with the source "Terragrunt".
func (f *TerragruntFeature) Diagnostics(path string) diagnostics.Diagnostics {
	diags := diagnostics.NewDiagnostics()

	record, err := f.store.TerragruntRecordByPath(path)
	if err != nil {
		return diags
	}

	for _, sourceDiags := range record.Diagnostics {
		for name, fileDiags := range sourceDiags {
			if _, ok := diags[name.String()]; !ok {
				diags[name.String()] = make(map[globalAst.DiagnosticSource]hcl.Diagnostics)
			}
			diags[name.String()][globalAst.TerragruntSource] = append(
				diags[name.String()][globalAst.TerragruntSource], fileDiags...)
		}
	}

	return diags
}

// Links returns the links of a Terragrunt file (see decoder.FileLinks).
func (f *TerragruntFeature) Links(dir, filename string) []fdecoder.Link {
	record, err := f.store.TerragruntRecordByPath(dir)
	if err != nil {
		return nil
	}
	file, ok := record.ParsedFiles[ast.Filename(filename)]
	if !ok {
		return nil
	}
	return fdecoder.FileLinks(f.fs, dir, filename, file)
}

// LinkTarget returns the file that the link at pos opens, as a
// definition target.
func (f *TerragruntFeature) LinkTarget(dir, filename string, pos hcl.Pos) (*decoder.ReferenceTarget, bool) {
	link, ok := fdecoder.LinkAt(f.Links(dir, filename), pos)
	if !ok {
		return nil, false
	}
	return &decoder.ReferenceTarget{
		OriginRange: link.Range,
		Path:        lang.Path{Path: filepath.Dir(link.Target), LanguageID: ast.LanguageOfFilename(filepath.Base(link.Target))},
		Range: hcl.Range{
			Filename: filepath.Base(link.Target),
			Start:    hcl.InitialPos,
			End:      hcl.InitialPos,
		},
	}, true
}
