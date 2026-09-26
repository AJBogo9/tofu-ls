// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-version"
	tfaddr "github.com/opentofu/registry-address"
)

// The requests below serve completion of required_providers entries.
// Each response is cached, failures included, so that typing does not
// repeat requests, and an offline registry costs one timeout at most per
// cachedFailureTTL.
const (
	topProvidersLimit = 500
	topProvidersTTL   = 6 * time.Hour
	providerDataTTL   = 30 * time.Minute
	cachedFailureTTL  = time.Minute
	defaultUserAgent  = "tofu-ls"
)

// TopProvider is a provider in the registry's list of popular providers.
type TopProvider struct {
	Addr       string `json:"addr"`
	Version    string `json:"version"`
	Popularity int64  `json:"popularity"`
}

// ProviderSearchResult is a provider found by the registry search.
type ProviderSearchResult struct {
	Addr        string `json:"addr"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

// TopProviders returns the registry's most popular providers, most popular
// first.
func (c Client) TopProviders(ctx context.Context) ([]TopProvider, error) {
	reqURL := fmt.Sprintf("%s/top/providers?limit=%d", c.BaseAPIURL, topProvidersLimit)
	v, err := c.cache.get(ctx, reqURL, topProvidersTTL, c.waitTimeout(), func(ctx context.Context) (any, error) {
		var providers []TopProvider
		err := c.getJSON(ctx, reqURL, &providers)
		return providers, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]TopProvider), nil
}

// SearchProviders returns the providers the registry search finds for
// query, in the order the registry ranks them. Modules and documents
// matching the query are left out.
func (c Client) SearchProviders(ctx context.Context, query string) ([]ProviderSearchResult, error) {
	reqURL := fmt.Sprintf("%s/registry/docs/search?q=%s", c.BaseAPIURL, url.QueryEscape(query))
	v, err := c.cache.get(ctx, reqURL, providerDataTTL, c.waitTimeout(), func(ctx context.Context) (any, error) {
		var results []ProviderSearchResult
		if err := c.getJSON(ctx, reqURL, &results); err != nil {
			return nil, err
		}
		providers := make([]ProviderSearchResult, 0, len(results))
		for _, r := range results {
			if r.Type == "provider" {
				providers = append(providers, r)
			}
		}
		return providers, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]ProviderSearchResult), nil
}

// ProviderVersions returns the versions of a provider which the registry
// has, newest first. It uses the provider registry protocol of the
// OpenTofu registry, so addresses on other hosts are an error.
func (c Client) ProviderVersions(ctx context.Context, addr tfaddr.Provider) (version.Collection, error) {
	if addr.Hostname != tfaddr.DefaultProviderRegistryHost {
		return nil, fmt.Errorf("provider %s is not in the default registry", addr.ForDisplay())
	}
	reqURL := fmt.Sprintf("%s/v1/providers/%s/%s/versions", c.BaseRegistryURL, addr.Namespace, addr.Type)
	v, err := c.cache.get(ctx, reqURL, providerDataTTL, c.waitTimeout(), func(ctx context.Context) (any, error) {
		var response providerVersionResponse
		if err := c.getJSON(ctx, reqURL, &response); err != nil {
			return nil, err
		}
		versions := make(version.Collection, 0, len(response.Versions))
		for _, pv := range response.Versions {
			if v, err := version.NewVersion(pv.Version); err == nil {
				versions = append(versions, v)
			}
		}
		sort.Sort(sort.Reverse(versions))
		return versions, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(version.Collection), nil
}

func (c Client) getJSON(ctx context.Context, reqURL string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	userAgent := c.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: c.waitTimeout()}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ClientError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// waitTimeout is how long a request may keep completion waiting: the
// client's timeout, which module version completion uses too.
func (c Client) waitTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// responseCache holds registry responses by URL. A request runs detached
// from the context of the completion that started it, so that a
// completion cancelled by further typing does not throw the response
// away: the next completion finds it cached.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	now     func() time.Time
}

type cacheEntry struct {
	done    chan struct{}
	value   any
	err     error
	expires time.Time
}

func newResponseCache() *responseCache {
	return &responseCache{
		entries: make(map[string]*cacheEntry),
		now:     time.Now,
	}
}

// get returns the cached value for key, or fetches it. It waits at most
// wait, or until ctx is done, for a fetch in progress. A nil cache
// fetches every time.
func (rc *responseCache) get(ctx context.Context, key string, ttl, wait time.Duration, fetch func(context.Context) (any, error)) (any, error) {
	if rc == nil {
		fetchCtx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		return fetch(fetchCtx)
	}

	rc.mu.Lock()
	e, ok := rc.entries[key]
	if ok {
		select {
		case <-e.done:
			if rc.now().After(e.expires) {
				ok = false
			}
		default:
			// in progress
		}
	}
	if !ok {
		e = &cacheEntry{done: make(chan struct{})}
		rc.entries[key] = e
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
			defer cancel()
			value, err := fetch(fetchCtx)
			ttlFor := ttl
			if err != nil {
				ttlFor = cachedFailureTTL
			}
			rc.mu.Lock()
			e.value, e.err, e.expires = value, err, rc.now().Add(ttlFor)
			rc.mu.Unlock()
			close(e.done)
		}()
	}
	rc.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-e.done:
		rc.mu.Lock()
		defer rc.mu.Unlock()
		return e.value, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("registry request %s: no response within %s", strings.SplitN(key, "?", 2)[0], wait)
	}
}
