// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"net/http"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	defaultBaseURL  = "https://api.opentofu.org"
	registryBaseURL = "https://registry.opentofu.org"
	defaultTimeout  = 5 * time.Second
	tracerName      = "github.com/opentofu/tofu-ls/internal/registry"
)

type Client struct {
	BaseAPIURL      string
	BaseRegistryURL string
	Timeout         time.Duration
	httpClient      *http.Client

	// UserAgent identifies the language server in the requests made for
	// provider completion.
	UserAgent string
	// cache holds the responses for provider completion. It is shared by
	// copies of the client.
	cache *responseCache
}

func NewClient() Client {
	client := cleanhttp.DefaultClient()
	client.Timeout = defaultTimeout
	client.Transport = otelhttp.NewTransport(client.Transport)

	return Client{
		BaseAPIURL:      defaultBaseURL,
		BaseRegistryURL: registryBaseURL,
		Timeout:         defaultTimeout,
		httpClient:      client,
		cache:           newResponseCache(),
	}
}
