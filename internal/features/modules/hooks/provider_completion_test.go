// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hooks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	tfaddr "github.com/opentofu/registry-address"
	"github.com/opentofu/tofu-ls/internal/features/modules/ast"
	"github.com/opentofu/tofu-ls/internal/features/modules/state"
	"github.com/opentofu/tofu-ls/internal/registry"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	"github.com/zclconf/go-cty/cty"
)

const topProvidersMockResponse = `[
	{"addr":"hashicorp/aws","version":"v6.66.0","popularity":11094},
	{"addr":"terraform-providers/aws","version":"v6.4.0","popularity":10408},
	{"addr":"hashicorp/google","version":"v7.0.0","popularity":2466},
	{"addr":"integrations/github","version":"v6.13.0","popularity":1144},
	{"addr":"hashicorp/awscc","version":"v1.103.0","popularity":500},
	{"addr":"hashicorp/google-beta","version":"v7.0.0","popularity":400},
	{"addr":"acme/aws","version":"v0.1.0","popularity":3}
]`

const randomVersionsMockResponse = `{"versions": [
	{"version": "3.6.0"},
	{"version": "3.7.0-alpha1"},
	{"version": "3.10.1"},
	{"version": "3.7.2"},
	{"version": "3.7.1"}
]}`

// providerHooksTestSetup parses src, where `|` marks the completion
// position, into a module and returns hooks and a completion context.
func providerHooksTestSetup(t *testing.T, src string, handler http.HandlerFunc) (*Hooks, context.Context, cty.Value, *[]string) {
	t.Helper()
	tmpDir := t.TempDir()

	offset := strings.Index(src, "|")
	src = strings.Replace(src, "|", "", 1)
	pos := hcl.Pos{
		Line:   strings.Count(src[:offset], "\n") + 1,
		Column: offset - strings.LastIndex(src[:offset], "\n"),
		Byte:   offset,
	}

	f, diags := hclsyntax.ParseConfig([]byte(src), "main.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	s, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewModuleStore(s.ProviderSchemas, s.RegistryModules, s.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(tmpDir); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateParsedModuleFiles(tmpDir, ast.ModFiles{"main.tf": f}, nil); err != nil {
		t.Fatal(err)
	}
	err = s.ProviderSchemas.AddPreloadedSchema(tfaddr.MustParseProviderSource("integrations/github"),
		version.Must(version.NewVersion("6.0.0")), &tfschema.ProviderSchema{})
	if err != nil {
		t.Fatal(err)
	}

	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	regClient := registry.NewClient()
	regClient.BaseAPIURL = srv.URL
	regClient.BaseRegistryURL = srv.URL

	h := &Hooks{
		ModStore:        store,
		RegistryClient:  regClient,
		ProviderSchemas: s.ProviderSchemas,
	}

	ctx := decoder.WithPath(context.Background(), lang.Path{Path: tmpDir, LanguageID: "opentofu"})
	ctx = decoder.WithPos(ctx, pos)
	ctx = decoder.WithFilename(ctx, "main.tf")
	ctx = decoder.WithMaxCandidates(ctx, 100)

	// the text between the opening quote and the position, as hcl-lang
	// passes it
	quote := strings.LastIndex(src[:offset], `"`)
	return h, ctx, cty.StringVal(src[quote+1 : offset]), &requests
}

func candidateLabels(candidates []decoder.Candidate) []string {
	labels := make([]string, len(candidates))
	for i, c := range candidates {
		labels[i] = c.Label
		if c.Detail != "" {
			labels[i] += " (" + c.Detail + ")"
		}
	}
	return labels
}

func TestHooks_ProviderSources(t *testing.T) {
	registryOK := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/top/providers":
			w.Write([]byte(topProvidersMockResponse))
		case "/registry/docs/search":
			if r.URL.Query().Get("q") == "trocco" {
				w.Write([]byte(`[{"type":"provider","addr":"trocco-io/trocco","version":"v0.36.0","description":"TROCCO"},
					{"type":"module","addr":"x/trocco/aws","version":"v1.0.0"}]`))
				return
			}
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}
	registryDown := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}

	testCases := []struct {
		name         string
		src          string
		handler      http.HandlerFunc
		want         []string
		wantSearches int
	}{
		{
			"nothing typed: providers named like the entry",
			`terraform {
  required_providers {
    aws = { source = "|" }
  }
}
`,
			registryOK,
			[]string{
				`"hashicorp/aws" (6.66.0)`,
				`"acme/aws" (0.1.0)`,
				`"hashicorp/awscc" (1.103.0)`,
			},
			// fewer than five popular matches: the search looks for more
			1,
		},
		{
			"a provider with a schema here comes first",
			`terraform {
  required_providers {
    github = {
      source = "|"
    }
  }
}
`,
			registryOK,
			[]string{`"integrations/github" (schema available (6.0.0))`},
			1,
		},
		{
			"namespace typed",
			`terraform {
  required_providers {
    google = { source = "hashicorp/g|" }
  }
}
`,
			registryOK,
			[]string{
				`"hashicorp/google" (7.0.0)`,
				`"hashicorp/google-beta" (7.0.0)`,
			},
			// one letter of the type is too short to search for
			0,
		},
		{
			"type typed",
			`terraform {
  required_providers {
    x = { source = "gith|" }
  }
}
`,
			registryOK,
			[]string{`"integrations/github" (6.13.0)`},
			1,
		},
		{
			"rare provider from the search",
			`terraform {
  required_providers {
    trocco = { source = "|" }
  }
}
`,
			registryOK,
			[]string{`"trocco-io/trocco" (0.36.0)`},
			1,
		},
		{
			"registry down: only providers with a schema",
			`terraform {
  required_providers {
    github = { source = "|" }
  }
}
`,
			registryDown,
			[]string{`"integrations/github" (schema available (6.0.0))`},
			1,
		},
		{
			"outside required_providers",
			`module "x" {
  source = "|"
}
`,
			registryOK,
			[]string{},
			0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, ctx, value, requests := providerHooksTestSetup(t, tc.src, tc.handler)
			candidates, err := h.ProviderSources(ctx, value)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, candidateLabels(candidates)); diff != "" {
				t.Fatalf("unexpected candidates: %s", diff)
			}
			searches := 0
			for _, r := range *requests {
				if strings.HasPrefix(r, "/registry/docs/search") {
					searches++
				}
			}
			if searches != tc.wantSearches {
				t.Fatalf("expected %d searches, got %d: %q", tc.wantSearches, searches, *requests)
			}
			for i, c := range candidates {
				if c.RawInsertText != c.Label || c.Kind != lang.StringCandidateKind {
					t.Fatalf("unexpected candidate %#v", c)
				}
				if want := fmt.Sprintf("%3d", i); c.SortText != want {
					t.Fatalf("candidate %d: sort text %q", i, c.SortText)
				}
			}
		})
	}
}

func TestHooks_ProviderVersions(t *testing.T) {
	registryOK := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/providers/hashicorp/random/versions":
			w.Write([]byte(randomVersionsMockResponse))
		default:
			http.NotFound(w, r)
		}
	}

	testCases := []struct {
		name    string
		src     string
		handler http.HandlerFunc
		want    []string
	}{
		{
			"nothing typed",
			`terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "|"
    }
  }
}
`,
			registryOK,
			[]string{
				`"~> 3.10" (newest hashicorp/random)`,
				`"3.10.1"`,
				`"~> 3.7"`,
				`"3.7.2"`,
				`"3.7.1"`,
				`"~> 3.6"`,
				`"3.6.0"`,
			},
		},
		{
			"pessimistic operator typed",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "~> |" }
  }
}
`,
			registryOK,
			[]string{
				`"~> 3.10" (newest hashicorp/random)`,
				`"~> 3.10.1"`,
				`"~> 3.7"`,
				`"~> 3.7.2"`,
				`"~> 3.7.1"`,
				`"~> 3.6"`,
				`"~> 3.6.0"`,
			},
		},
		{
			"implied source, second constraint of a list",
			`terraform {
  required_providers {
    random = { version = ">= 3.0,<|" }
  }
}
`,
			registryOK,
			[]string{
				`">= 3.0, < 3.10.1" (newest hashicorp/random)`,
				`">= 3.0, < 3.7.2"`,
				`">= 3.0, < 3.7.1"`,
				`">= 3.0, < 3.6.0"`,
			},
		},
		{
			"version typed narrows",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.7|" }
  }
}
`,
			registryOK,
			[]string{
				`"~> 3.7" (newest hashicorp/random)`,
				`"3.7.2"`,
				`"3.7.1"`,
			},
		},
		{
			"pre-releases when a dash is typed",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.7.0-|" }
  }
}
`,
			registryOK,
			[]string{`"3.7.0-alpha1" (newest hashicorp/random)`},
		},
		{
			"registry down",
			`terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "|" }
  }
}
`,
			func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "down", http.StatusBadGateway)
			},
			[]string{},
		},
		{
			"provider on another host",
			`terraform {
  required_providers {
    random = { source = "example.com/acme/random", version = "|" }
  }
}
`,
			registryOK,
			[]string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, ctx, value, _ := providerHooksTestSetup(t, tc.src, tc.handler)
			candidates, err := h.ProviderVersions(ctx, value)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, candidateLabels(candidates)); diff != "" {
				t.Fatalf("unexpected candidates: %s", diff)
			}
		})
	}
}
