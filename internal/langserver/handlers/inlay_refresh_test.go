// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/creachadair/jrpc2"
	"github.com/opentofu/tofu-ls/internal/settings"
)

type recordingServer struct {
	mu      sync.Mutex
	methods []string
}

func (s *recordingServer) Notify(ctx context.Context, method string, params interface{}) error {
	return nil
}

func (s *recordingServer) Callback(ctx context.Context, method string, params interface{}) (*jrpc2.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	return nil, nil
}

func (s *recordingServer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.methods...)
}

func TestScheduleInlayHintRefresh(t *testing.T) {
	testCases := []struct {
		name      string
		supported bool
		values    bool
		want      int
	}{
		{"client supports refresh", true, true, 1},
		{"client without refresh support", false, true, 0},
		{"value hints turned off", true, false, 0},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &recordingServer{}
			svc := &service{
				logger:           log.New(io.Discard, "", 0),
				sessCtx:          context.Background(),
				server:           srv,
				inlayHintRefresh: tc.supported,
				inlayHints:       settings.InlayHints{Values: tc.values},
			}
			// a burst of edits asks for one refresh
			for i := 0; i < 5; i++ {
				svc.scheduleInlayHintRefresh()
			}
			time.Sleep(inlayHintRefreshDelay + 300*time.Millisecond)
			got := srv.calls()
			if len(got) != tc.want {
				t.Fatalf("expected %d refresh requests, got %q", tc.want, got)
			}
			for _, m := range got {
				if m != "workspace/inlayHint/refresh" {
					t.Fatalf("unexpected request %q", m)
				}
			}
		})
	}
}
