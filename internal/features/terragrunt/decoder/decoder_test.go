// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package decoder_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	lsctx "github.com/opentofu/tofu-ls/internal/context"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/terragrunt/decoder"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/jobs"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/filesystem"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }

const appUnit = `include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

include "common" {
  path   = "${get_repo_root()}/common/app.hcl"
  expose = true
}

include "hidden" {
  path = "${get_repo_root()}/common/app.hcl"
}

locals {
  name = "app-${include.common.locals.service}"
}

dependency "vpc" {
  config_path = "../vpc"
}

dependencies {
  paths = ["../vpc", "../missing"]
}

feature "fast" {
  default = false
}

terraform {
  source = "../../modules//vpc"
}

inputs = {
  vpc_id  = dependency.vpc.outputs.vpc_id
  name    = local.name
  account = include.root.locals.account
  fast    = feature.fast.value
  hidden  = include.hidden.locals.service
  other   = read_terragrunt_config("../vpc")
}
`

const rootConfig = `locals {
  account_vars = read_terragrunt_config(find_in_parent_folders("account.hcl"))
  account      = "acme"
}

terraform {
  source = "../modules/vpc"
}
`

const stackFile = `unit "api" {
  source = "../catalog/units/api"
  path   = "api"

  autoinclude {
    dependency "db" {
      config_path = unit.db.path
    }
  }
}

unit "db" {
  source = "github.com/acme/catalog//units/db"
  path   = "db"
}
`

// writeTree writes a Terragrunt repository into a temporary directory.
func writeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"root.hcl":                         rootConfig,
		"common/app.hcl":                   "locals {\n  service = \"api\"\n}\n",
		"modules/vpc/main.tf":              "variable \"cidr\" {}\n",
		"live/app/terragrunt.hcl":          appUnit,
		"live/vpc/terragrunt.hcl":          "terraform {\n  source = \"../../modules//vpc\"\n}\n",
		"catalog/units/api/terragrunt.hcl": "inputs = {}\n",
		"stack/terragrunt.stack.hcl":       stackFile,
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// index parses and decodes the Terragrunt files of the directories.
func index(t *testing.T, dirs ...string) *state.TerragruntStore {
	t.Helper()
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})
	gs, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewTerragruntStore(gs.ChangeStore)
	if err != nil {
		t.Fatal(err)
	}
	fs := filesystem.NewFilesystem(gs.DocumentStore)
	for _, dir := range dirs {
		if err := store.Add(dir); err != nil {
			t.Fatal(err)
		}
		if err := jobs.ParseTerragruntFiles(ctx, fs, store, dir, nil); err != nil {
			t.Fatal(err)
		}
		if err := jobs.DecodeTerragruntReferences(ctx, store, dir); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// posOf returns the position of the nth (1-based) needle on the line,
// plus offset characters.
func posOf(t *testing.T, src string, line int, needle string, nth, offset int) hcl.Pos {
	t.Helper()
	lines := strings.Split(src, "\n")
	text := lines[line-1]
	col := -1
	for i := 0; i < nth; i++ {
		next := strings.Index(text[col+1:], needle)
		if next < 0 {
			t.Fatalf("needle %q #%d not on line %d: %q", needle, nth, line, text)
		}
		col += next + 1
	}
	byteOffset := 0
	for _, l := range lines[:line-1] {
		byteOffset += len(l) + 1
	}
	return hcl.Pos{Line: line, Column: col + offset + 1, Byte: byteOffset + col + offset}
}

func TestTerragrunt_references(t *testing.T) {
	root := writeTree(t)
	app := filepath.Join(root, "live", "app")
	stack := filepath.Join(root, "stack")
	store := index(t, app, stack)
	d := decoder.NewDecoder(&fdecoder.PathReader{StateReader: store, FS: osFS{}, Includes: fdecoder.NewIncludeCache()})
	unitPath := lang.Path{Path: app, LanguageID: "terragrunt"}
	stackPath := lang.Path{Path: stack, LanguageID: "terragrunt-stack"}

	testCases := []struct {
		name   string
		path   lang.Path
		file   string
		src    string
		pos    hcl.Pos
		target string // file relative to root, and line
	}{
		{"local", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 37, "name", 2, 1), "live/app/terragrunt.hcl:16"},
		{"dependency outputs", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 36, "outputs", 1, 1), "live/app/terragrunt.hcl:19"},
		{"feature", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 39, "fast", 2, 1), "live/app/terragrunt.hcl:27"},
		{"local of an included file", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 38, "account", 2, 1), "root.hcl:3"},
		{"include found by get_repo_root", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 16, "service", 1, 1), "common/app.hcl:2"},
		{"the include block", unitPath, "terragrunt.hcl", appUnit, posOf(t, appUnit, 38, "root", 1, 1), "live/app/terragrunt.hcl:1"},
		{"a unit of a stack", stackPath, "terragrunt.stack.hcl", stackFile, posOf(t, stackFile, 7, "db", 1, 1), "stack/terragrunt.stack.hcl:12"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := d.ReferenceTargetsForOriginAtPos(tc.path, tc.file, tc.pos)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for _, target := range targets {
				rel, err := filepath.Rel(root, filepath.Join(target.Path.Path, target.Range.Filename))
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, filepath.ToSlash(rel)+":"+strconv.Itoa(target.Range.Start.Line))
			}
			if diff := cmp.Diff([]string{tc.target}, got); diff != "" {
				t.Fatalf("targets: %s", diff)
			}
		})
	}

	// an include without expose = true exposes nothing
	targets, err := d.ReferenceTargetsForOriginAtPos(unitPath, "terragrunt.hcl", posOf(t, appUnit, 40, "service", 1, 1))
	if err == nil && len(targets) > 0 {
		t.Errorf("include.hidden is not exposed, got targets %v", targets)
	}

	hovers := []struct {
		name string
		pos  hcl.Pos
		want []string
	}{
		{"dependency", posOf(t, appUnit, 36, "vpc", 2, 1), []string{"`dependency.vpc`", "Dependency `vpc` on `\"../vpc\"`", "`outputs` holds its outputs"}},
		{"included local", posOf(t, appUnit, 38, "account", 2, 1), []string{"`include.root.locals.account`", "Local value `account` of `../../root.hcl`: `account = \"acme\"`"}},
		{"feature", posOf(t, appUnit, 39, "fast", 2, 1), []string{"Feature flag `fast`", "`false` (its default)"}},
		{"function", posOf(t, appUnit, 2, "find_in", 1, 1), []string{"find_in_parent_folders(name string, fallback string) string", "docs.terragrunt.com/reference/hcl/functions"}},
		{"block", posOf(t, appUnit, 19, "dependency", 1, 1), []string{"**dependency** _Block_", "docs.terragrunt.com/reference/hcl/blocks/#dependency"}},
	}
	pd, err := d.Path(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range hovers {
		t.Run("hover "+tc.name, func(t *testing.T) {
			hover, err := pd.HoverAtPos(context.Background(), "terragrunt.hcl", tc.pos)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(hover.Content.Value, want) {
					t.Errorf("hover lacks %q:\n%s", want, hover.Content.Value)
				}
			}
		})
	}
}

func TestFileLinks(t *testing.T) {
	root := writeTree(t)
	app := filepath.Join(root, "live", "app")
	store := index(t, app, root, filepath.Join(root, "stack"))

	links := func(dir, file string) []string {
		record, err := store.TerragruntRecordByPath(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0)
		for _, link := range fdecoder.FileLinks(osFS{}, dir, file, record.ParsedFiles[ast.Filename(file)]) {
			rel, err := filepath.Rel(root, link.Target)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{strconv.Itoa(link.Range.Start.Line), filepath.ToSlash(rel), link.Tooltip}, " "))
		}
		return got
	}

	want := []string{
		// include path from find_in_parent_folders, walking up
		"2 root.hcl Open ../../root.hcl",
		// include path through get_repo_root
		"7 common/app.hcl Open ../../common/app.hcl",
		"12 common/app.hcl Open ../../common/app.hcl",
		// dependency config_path: the unit's configuration
		"20 live/vpc/terragrunt.hcl Open ../vpc/terragrunt.hcl",
		// dependencies.paths, without the missing one
		"24 live/vpc/terragrunt.hcl Open ../vpc/terragrunt.hcl",
		// a local source: the module's main.tf
		"32 modules/vpc/main.tf Open ../../modules/vpc/main.tf",
		// read_terragrunt_config of a directory
		"41 live/vpc/terragrunt.hcl Open ../vpc/terragrunt.hcl",
	}
	if diff := cmp.Diff(want, links(app, "terragrunt.hcl")); diff != "" {
		t.Errorf("unit links: %s", diff)
	}

	// root.hcl is included: its relative paths and find_in_parent_folders
	// depend on the unit that includes it
	if got := links(root, "root.hcl"); len(got) != 0 {
		t.Errorf("root.hcl links: %v", got)
	}

	// a unit's local source in a stack
	if diff := cmp.Diff([]string{"2 catalog/units/api/terragrunt.hcl Open ../catalog/units/api/terragrunt.hcl"},
		links(filepath.Join(root, "stack"), "terragrunt.stack.hcl")); diff != "" {
		t.Errorf("stack links: %s", diff)
	}
}
