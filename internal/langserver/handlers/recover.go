// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"context"
	"runtime/debug"

	"github.com/creachadair/jrpc2"
	rpch "github.com/creachadair/jrpc2/handler"
)

// recoverHandlers wraps every handler of m, requests, notifications and
// the commands of workspace/executeCommand alike, so that a panic while
// serving one is logged with its stack and answered with an internal
// error. Without it one bad request ends the server, and after a few
// restarts the client gives up on it for the rest of the session.
func (svc *service) recoverHandlers(m rpch.Map) {
	for method, h := range m {
		m[method] = svc.recoverHandler(method, h)
	}
}

func (svc *service) recoverHandler(method string, h jrpc2.Handler) jrpc2.Handler {
	return func(ctx context.Context, req *jrpc2.Request) (result any, err error) {
		defer func() {
			if r := recover(); r != nil {
				svc.logger.Printf("panic while serving %q: %v\n%s", method, r, debug.Stack())
				result = nil
				err = jrpc2.Errorf(jrpc2.InternalError, "internal error in %s: %v", method, r)
			}
		}()
		return h(ctx, req)
	}
}
