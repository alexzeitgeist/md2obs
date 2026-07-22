// Package watcher provides event-driven, non-recursive watching of the
// immediate parent directories of explicitly selected source files.
package watcher

import (
	"path/filepath"
	"sort"
)

// Index is the exact-path filter for one watch session. Startup candidates and
// later explicit imports can be enrolled; every other file appearing in an
// armed directory is ignored by construction.
type Index struct {
	sources    map[string]struct{}
	parentRefs map[string]int
}

// NewIndex builds the exact-path set and the deduplicated list of immediate
// parent directories to watch.
func NewIndex(paths []string) *Index {
	ix := &Index{
		sources:    make(map[string]struct{}, len(paths)),
		parentRefs: make(map[string]int),
	}
	for _, p := range paths {
		ix.Add(p)
	}
	return ix
}

// Add enrolls one exact source path. It reports whether the path was new.
// Index is intentionally not synchronized: one watcher event-loop goroutine
// owns both additions and matches.
func (ix *Index) Add(path string) bool {
	clean := filepath.Clean(path)
	if _, ok := ix.sources[clean]; ok {
		return false
	}
	ix.sources[clean] = struct{}{}
	ix.parentRefs[filepath.Dir(clean)]++
	return true
}

// Remove stops matching a source until a later membership refresh adds it. It
// also releases the parent from the index when no selected source still uses
// that directory.
func (ix *Index) Remove(path string) bool {
	clean := filepath.Clean(path)
	if _, ok := ix.sources[clean]; !ok {
		return false
	}
	delete(ix.sources, clean)

	parent := filepath.Dir(clean)
	ix.parentRefs[parent]--
	if ix.parentRefs[parent] == 0 {
		delete(ix.parentRefs, parent)
	}
	return true
}

// HasParent reports whether any currently selected source uses parent.
func (ix *Index) HasParent(parent string) bool {
	_, ok := ix.parentRefs[filepath.Clean(parent)]
	return ok
}

// Paths returns the currently selected exact source paths in sorted order.
func (ix *Index) Paths() []string {
	paths := make([]string, 0, len(ix.sources))
	for path := range ix.sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Has reports whether an event path is one of the selected sources.
func (ix *Index) Has(eventPath string) bool {
	_, ok := ix.sources[filepath.Clean(eventPath)]
	return ok
}

// Parents returns the distinct immediate parent directories, sorted.
func (ix *Index) Parents() []string {
	parents := make([]string, 0, len(ix.parentRefs))
	for parent := range ix.parentRefs {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	return parents
}

// Len returns the number of selected sources.
func (ix *Index) Len() int { return len(ix.sources) }
