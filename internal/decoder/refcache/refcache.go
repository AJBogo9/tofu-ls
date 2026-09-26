// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package refcache keeps the reference contexts of paths across
// requests (see decoder.ReferencePathReader).
package refcache

import (
	"sync"
	"weak"

	"github.com/hashicorp/hcl-lang/decoder"
)

// Cache keeps the reference context of each path together with the state
// record it was built from. A record is replaced, never changed, when its
// module changes, so a context is rebuilt exactly when its record is new.
// The records are held weakly: a replaced record is not kept alive.
//
// A nil *Cache caches nothing. A Cache is safe for concurrent use.
type Cache[R any] struct {
	mu      sync.Mutex
	entries map[string]entry[R]
}

type entry[R any] struct {
	record  weak.Pointer[R]
	pathCtx *decoder.PathContext
}

func New[R any]() *Cache[R] {
	return &Cache[R]{entries: make(map[string]entry[R])}
}

// Get returns the context of the path built from record, calling build
// only when no context of that record is cached.
func (c *Cache[R]) Get(path string, record *R, build func(*R) *decoder.PathContext) *decoder.PathContext {
	if c == nil {
		return build(record)
	}
	key := weak.Make(record)

	c.mu.Lock()
	e, ok := c.entries[path]
	c.mu.Unlock()
	if ok && e.record == key {
		return e.pathCtx
	}

	pathCtx := build(record)

	c.mu.Lock()
	c.entries[path] = entry[R]{record: key, pathCtx: pathCtx}
	c.mu.Unlock()

	return pathCtx
}

// Forget drops the context of a path, e.g. once its record is removed.
func (c *Cache[R]) Forget(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, path)
	c.mu.Unlock()
}
