// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package langserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/creachadair/jrpc2"
)

type rpcLogger struct {
	logger *log.Logger
}

func (rl *rpcLogger) LogRequest(ctx context.Context, req *jrpc2.Request) {
	idStr := ""
	if req.ID() != "" {
		idStr = fmt.Sprintf(" (ID %s)", req.ID())
	}
	reqType := "request"
	if req.IsNotification() {
		reqType = "notification"
	}

	var params json.RawMessage
	req.UnmarshalParams(&params)

	rl.logger.Printf("Incoming %s for %q%s: %s",
		reqType, req.Method(), idStr, params)
}

func (rl *rpcLogger) LogResponse(ctx context.Context, rsp *jrpc2.Response) {
	idStr := ""
	if rsp.ID() != "" {
		idStr = fmt.Sprintf(" (ID %s)", rsp.ID())
	}

	// jrpc2 answers a message it could not accept as a request, such as
	// a client's late reply to a callback, with no context
	method := ""
	if ctx != nil {
		if req := jrpc2.InboundRequest(ctx); req != nil {
			method = req.Method()
			if req.IsNotification() {
				idStr = " (notification)"
			}
		}
	}

	if rsp.Error() != nil {
		rl.logger.Printf("Error for %q%s: %s", method, idStr, rsp.Error())
		return
	}
	var body json.RawMessage
	rsp.UnmarshalResult(&body)
	rl.logger.Printf("Response to %q%s: %s", method, idStr, body)
}
