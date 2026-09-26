// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/filepaths"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/uri"
	"github.com/zclconf/go-cty/cty"
)

// pathResolver works out the files that file function arguments of one
// module name. It loads the static evaluator only for arguments that
// need it: references to variables or locals, or a path relative to the
// root module of a module that may be called by another one.
type pathResolver struct {
	svc *service
	ctx context.Context
	dir string

	ev       *staticval.Evaluator
	evLoaded bool
}

func (r *pathResolver) evaluator() *staticval.Evaluator {
	if !r.evLoaded {
		r.evLoaded = true
		ev, err := r.svc.staticEvaluator(r.ctx, r.dir, nil)
		if err == nil {
			r.ev = ev
		}
	}
	return r.ev
}

// resolve returns the absolute path that expr names, when it is static.
func (r *pathResolver) resolve(expr hclsyntax.Expression) (string, bool) {
	if path, anchored, ok := filepaths.StaticPath(expr); ok {
		rootDir := ""
		if !anchored {
			if ev := r.evaluator(); ev != nil {
				rootDir = ev.Module().RootPath
			}
		}
		return filepaths.Resolve(path, anchored, r.dir, rootDir), true
	}

	ev := r.evaluator()
	if ev == nil {
		return "", false
	}
	res := ev.Eval(expr, nil)
	if !res.IsKnown() || res.IsSensitive() {
		return "", false
	}
	v := res.Value
	if v.IsNull() || v.Type() != cty.String || v.AsString() == "" {
		return "", false
	}
	// the evaluator makes path.module relative to the root module
	return filepaths.Resolve(v.AsString(), false, r.dir, ev.Module().RootPath), true
}

// existingFile returns the path that call names when it is a file that
// exists.
func (r *pathResolver) existingFile(call filepaths.Call) (string, bool) {
	if call.Kind == filepaths.Directory {
		return "", false
	}
	path, ok := r.resolve(call.Path)
	if !ok {
		return "", false
	}
	fi, err := r.svc.fs.Stat(path)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return path, true
}

func (svc *service) parsedDocumentFile(doc *document.Document) (*hcl.File, bool) {
	if ilsp.ParseLanguageID(doc.LanguageID) != ilsp.OpenTofu {
		return nil, false
	}
	files, ok := svc.moduleFiles(doc.Dir.Path())
	if !ok {
		return nil, false
	}
	f, ok := files[doc.Filename]
	return f, ok
}

// filePathTarget returns the file that a file function's path argument at
// pos names, as a definition target.
func (svc *service) filePathTarget(ctx context.Context, doc *document.Document, pos hcl.Pos) (*decoder.ReferenceTarget, bool) {
	f, ok := svc.parsedDocumentFile(doc)
	if !ok {
		return nil, false
	}
	call, ok := filepaths.CallAtPos(f.Body, pos)
	if !ok {
		return nil, false
	}
	r := &pathResolver{svc: svc, ctx: ctx, dir: doc.Dir.Path()}
	path, ok := r.existingFile(call)
	if !ok {
		return nil, false
	}
	return &decoder.ReferenceTarget{
		OriginRange: filepaths.TextRange(call.Path, f.Bytes),
		Path:        lang.Path{Path: filepath.Dir(path), LanguageID: ilsp.OpenTofu.String()},
		Range: hcl.Range{
			Filename: filepath.Base(path),
			Start:    hcl.InitialPos,
			End:      hcl.InitialPos,
		},
	}, true
}

// filePathLinks returns a link for each file function path argument in
// the document that names an existing file.
func (svc *service) filePathLinks(ctx context.Context, doc *document.Document) []lang.Link {
	f, ok := svc.parsedDocumentFile(doc)
	if !ok {
		return nil
	}
	r := &pathResolver{svc: svc, ctx: ctx, dir: doc.Dir.Path()}
	links := make([]lang.Link, 0)
	for _, call := range filepaths.Calls(f.Body) {
		path, ok := r.existingFile(call)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(doc.Dir.Path(), path)
		if err != nil {
			rel = path
		}
		links = append(links, lang.Link{
			URI:     uri.FromPath(path),
			Tooltip: fmt.Sprintf("Open %s", filepath.ToSlash(rel)),
			Range:   filepaths.TextRange(call.Path, f.Bytes),
		})
	}
	return links
}

// preferredExtensions are the file extensions offered first for a
// function's path.
var preferredExtensions = map[string][]string{
	"templatefile": {".tftpl", ".tpl"},
}

// relativePathBase returns the directory that OpenTofu resolves a bare
// relative file path of the module in dir from, the working directory:
// the module's own directory when it is run as a root module, else the
// directory of the root module that calls it when exactly one does. It
// returns false when several roots, or roots that are not known, call it.
func (svc *service) relativePathBase(dir string) (string, bool) {
	fsys := svc.staticFS()
	mod, err := staticval.LoadModule(fsys, dir)
	if err != nil {
		return "", false
	}
	staticval.AddCallers(fsys, mod, maxCallerNesting, svc.indexedCallers)
	if mod.RootPath == "" {
		return dir, true
	}
	roots, ok := staticval.CallingRoots(mod)
	if !ok || len(roots) != 1 {
		return "", false
	}
	return roots[0], true
}

// filePathCompletion completes the path in the string literal of a file
// function's path argument at pos: the files and directories of the
// directory typed so far, relative to the module directory for a path
// that starts with path.module, else relative to the directory OpenTofu
// resolves it from (see relativePathBase). Directories come after the
// files a function usually reads, and fileset gets directories only.
func (svc *service) filePathCompletion(doc *document.Document, pos hcl.Pos) (lsp.CompletionList, bool) {
	list := lsp.CompletionList{Items: []lsp.CompletionItem{}}
	if !bytes.Contains(doc.Text, []byte("file")) {
		// every file function has "file" in its name
		return list, false
	}
	f, ok := svc.parsedDocumentFile(doc)
	if !ok {
		return list, false
	}
	call, ok := filepaths.CallAtPos(f.Body, pos)
	if !ok {
		return list, false
	}
	typed, _, anchored, ok := filepaths.LiteralAtPos(call.Path, f.Bytes, pos)
	if !ok {
		return list, false
	}

	prefixSlash := ""
	if anchored {
		// "${path.module}/..." relative to the module directory
		if typed == "" {
			prefixSlash = "/"
		}
		typed = strings.TrimPrefix(typed, "/")
	}
	dirPart, partial := "", typed
	if i := strings.LastIndex(typed, "/"); i >= 0 {
		dirPart, partial = typed[:i+1], typed[i+1:]
	}
	var listDir string
	fromRoot := ""
	if filepath.IsAbs(dirPart) {
		listDir = filepath.FromSlash(dirPart)
	} else {
		base := doc.Dir.Path()
		if !anchored {
			var ok bool
			base, ok = svc.relativePathBase(base)
			if !ok {
				return list, true
			}
			if base != doc.Dir.Path() {
				if rel, err := filepath.Rel(doc.Dir.Path(), base); err == nil {
					fromRoot = filepath.ToSlash(rel)
				} else {
					fromRoot = base
				}
			}
		}
		listDir = filepath.Join(base, filepath.FromSlash(dirPart))
	}
	entries, err := svc.fs.ReadDir(listDir)
	if err != nil {
		return list, true
	}

	// the edit replaces the path segment being typed, up to the next
	// separator or the end of the literal
	end := pos.Byte
	for end < len(f.Bytes) && !strings.ContainsRune("\"/$\n", rune(f.Bytes[end])) {
		end++
	}
	editRange := hcl.Range{
		Filename: doc.Filename,
		Start:    hcl.Pos{Line: pos.Line, Byte: pos.Byte - len(partial)},
		End:      hcl.Pos{Line: pos.Line, Byte: end},
	}
	lspRange := ilsp.HCLRangeToLSPInText(editRange, doc.Text)

	type pathItem struct {
		name  string
		isDir bool
		rank  int
	}
	items := make([]pathItem, 0)
	preferred := preferredExtensions[call.Function]
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasPrefix(name, partial) {
			continue
		}
		isDir := e.IsDir()
		if call.Kind == filepaths.Directory && !isDir {
			continue
		}
		rank := 1
		switch {
		case isDir:
			rank = 2
		case len(preferred) == 0:
			rank = 0
		default:
			for _, ext := range preferred {
				if strings.HasSuffix(name, ext) {
					rank = 0
				}
			}
		}
		items = append(items, pathItem{name: name, isDir: isDir, rank: rank})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return items[i].name < items[j].name
	})

	for i, it := range items {
		text := prefixSlash + it.name
		detail := filepath.ToSlash(filepath.Join(dirPart, it.name))
		if fromRoot != "" {
			detail += " (in the root module " + fromRoot + ")"
		}
		kind := lsp.FileCompletion
		var command *lsp.Command
		if it.isDir {
			kind = lsp.FolderCompletion
			if call.Kind != filepaths.Directory {
				text += "/"
				command = &lsp.Command{Command: "editor.action.triggerSuggest", Title: "Suggest"}
			}
		}
		list.Items = append(list.Items, lsp.CompletionItem{
			Label:    it.name,
			Kind:     kind,
			Detail:   detail,
			SortText: fmt.Sprintf("%d%4d", it.rank, i),
			TextEdit: &lsp.TextEdit{
				Range:   lspRange,
				NewText: text,
			},
			FilterText: it.name,
			Command:    command,
		})
	}
	return list, true
}
