// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package eventbus

import (
	"context"

	"github.com/opentofu/tofu-ls/internal/document"
)

// ProviderSchemasChangeEvent is an event that should be fired whenever
// the provider schemas of a root module were obtained (via tofu
// providers schema), for example after the first open or after tofu
// init, so that what was checked without them is checked again.
type ProviderSchemasChangeEvent struct {
	// Context is the context of the request that asked for the schemas
	Context context.Context

	Dir document.DirHandle
}

func (n *EventBus) OnProviderSchemasChange(identifier string, doneChannel <-chan struct{}) <-chan ProviderSchemasChangeEvent {
	n.logger.Printf("bus: %q subscribed to OnProviderSchemasChange", identifier)
	return n.providerSchemasChangeTopic.Subscribe(doneChannel)
}

func (n *EventBus) ProviderSchemasChange(e ProviderSchemasChangeEvent) {
	n.logger.Printf("bus: -> ProviderSchemasChange %s", e.Dir)
	n.providerSchemasChangeTopic.Publish(e)
}
