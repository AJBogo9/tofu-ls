// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/handler"
	"github.com/opentofu/tofu-ls/internal/langserver"
	"github.com/opentofu/tofu-ls/internal/state"
	"github.com/opentofu/tofu-ls/internal/walker"
)

func TestLangServer_panicInHandler(t *testing.T) {
	tmpDir := TempDir(t)

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	wc := walker.NewWalkerCollector()

	var nilMap map[string]string
	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		AdditionalHandlers: map[string]handler.Func{
			"$/panicNilMap": func(ctx context.Context, req *jrpc2.Request) (interface{}, error) {
				nilMap["x"] = "y"
				return nil, nil
			},
			"$/panicValue": func(ctx context.Context, req *jrpc2.Request) (interface{}, error) {
				panic("injected")
			},
		},
		StateStore:      ss,
		WalkerCollector: wc,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
	    "capabilities": {},
	    "rootUri": %q,
	    "processId": 12345
	}`, tmpDir.URI)})
	waitForWalkerPath(t, ss, wc, tmpDir)
	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})

	testCases := []struct {
		method  string
		message string
	}{
		{"$/panicNilMap", "internal error in $/panicNilMap: assignment to entry in nil map"},
		{"$/panicValue", "internal error in $/panicValue: injected"},
		// the server survived the first panic
		{"$/panicValue", "internal error in $/panicValue: injected"},
	}
	for _, tc := range testCases {
		ls.CallAndExpectError(t, &langserver.CallRequest{
			Method:    tc.method,
			ReqParams: "{}",
		}, jrpc2.Errorf(jrpc2.InternalError, "%s", tc.message))
	}

	// a notification that panics has no reply, and the server lives on
	ls.Notify(t, &langserver.CallRequest{
		Method:    "$/panicValue",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method:    "workspace/symbol",
		ReqParams: `{"query": ""}`,
	})
}
