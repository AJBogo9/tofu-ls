// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	tfaddr "github.com/opentofu/registry-address"
)

func testCompletionClient(t *testing.T, handler http.HandlerFunc) (Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client := NewClient()
	client.BaseAPIURL = srv.URL
	client.BaseRegistryURL = srv.URL
	client.UserAgent = "tofu-ls/0.0.0-test"
	return client, &requests
}

func TestProviderVersions(t *testing.T) {
	var userAgent string
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/providers/hashicorp/random/versions" {
			http.NotFound(w, r)
			return
		}
		userAgent = r.Header.Get("User-Agent")
		w.Write([]byte(`{"versions": [
			{"version": "3.6.0", "protocols": ["5.0"]},
			{"version": "3.7.0-alpha1"},
			{"version": "3.10.1"},
			{"version": "not-a-version"},
			{"version": "3.7.2"}
		]}`))
	})
	addr := tfaddr.MustParseProviderSource("hashicorp/random")

	for i := 0; i < 2; i++ {
		versions, err := client.ProviderVersions(context.Background(), addr)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(versions))
		for i, v := range versions {
			got[i] = v.Original()
		}
		want := []string{"3.10.1", "3.7.2", "3.7.0-alpha1", "3.6.0"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("unexpected versions: %s", diff)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("expected 1 request (the second call is cached), got %d", n)
	}
	if userAgent != "tofu-ls/0.0.0-test" {
		t.Fatalf("unexpected user agent %q", userAgent)
	}
}

func TestProviderVersions_otherHost(t *testing.T) {
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {})
	addr := tfaddr.MustParseProviderSource("example.com/acme/thing")

	if _, err := client.ProviderVersions(context.Background(), addr); err == nil {
		t.Fatal("expected an error for a provider outside the default registry")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("expected no request, got %d", n)
	}
}

func TestTopProviders_failureIsCached(t *testing.T) {
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})

	for i := 0; i < 3; i++ {
		_, err := client.TopProviders(context.Background())
		var clientErr ClientError
		if !errors.As(err, &clientErr) || clientErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected the 503 error, got %v", err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("expected 1 request, got %d", n)
	}
}

func TestTopProviders_expires(t *testing.T) {
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/top/providers" || r.URL.Query().Get("limit") != "500" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"addr":"hashicorp/aws","version":"v6.0.0","popularity":10}]`))
	})
	now := time.Now()
	client.cache.now = func() time.Time { return now }

	providers, err := client.TopProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expected := []TopProvider{{Addr: "hashicorp/aws", Version: "v6.0.0", Popularity: 10}}
	if diff := cmp.Diff(expected, providers); diff != "" {
		t.Fatalf("unexpected providers: %s", diff)
	}
	if _, err := client.TopProviders(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(topProvidersTTL + time.Second)
	if _, err := client.TopProviders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("expected 2 requests (one after expiry), got %d", n)
	}
}

func TestSearchProviders(t *testing.T) {
	var query string
	client, _ := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("q")
		w.Write([]byte(`[
			{"type":"provider","addr":"integrations/github","version":"v6.13.0","description":"GitHub"},
			{"type":"module","addr":"stakater/github/module","version":"v1.0.12"},
			{"type":"provider/resource","addr":"integrations/github","version":"v6.13.0"},
			{"type":"provider","addr":"kuwas/github","version":"v4.3.0"}
		]`))
	})

	results, err := client.SearchProviders(context.Background(), "git hub")
	if err != nil {
		t.Fatal(err)
	}
	expected := []ProviderSearchResult{
		{Addr: "integrations/github", Version: "v6.13.0", Description: "GitHub", Type: "provider"},
		{Addr: "kuwas/github", Version: "v4.3.0", Type: "provider"},
	}
	if diff := cmp.Diff(expected, results); diff != "" {
		t.Fatalf("unexpected results: %s", diff)
	}
	if query != "git hub" {
		t.Fatalf("unexpected query %q", query)
	}
}

func TestProviderVersions_slowRegistry(t *testing.T) {
	release := make(chan struct{})
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(`{"versions": [{"version": "1.0.0"}]}`))
	})
	client.Timeout = 50 * time.Millisecond
	addr := tfaddr.MustParseProviderSource("hashicorp/random")

	start := time.Now()
	_, err := client.ProviderVersions(context.Background(), addr)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("completion waited %s, longer than the client timeout", elapsed)
	}
	close(release)
	if n := requests.Load(); n != 1 {
		t.Fatalf("expected 1 request, got %d", n)
	}
}

func TestProviderVersions_cancelledCompletionKeepsResponse(t *testing.T) {
	release := make(chan struct{})
	client, requests := testCompletionClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{"versions": [{"version": "1.0.0"}]}`))
	})
	addr := tfaddr.MustParseProviderSource("hashicorp/random")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := client.ProviderVersions(ctx, addr)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation, got %v", err)
	}
	close(release)

	versions, err := client.ProviderVersions(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].String() != "1.0.0" {
		t.Fatalf("unexpected versions %v", versions)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("expected the cancelled request's response to be reused, got %d requests", n)
	}
}
