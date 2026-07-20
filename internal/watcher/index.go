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
	sources   map[string]struct{}
	parentSet map[string]struct{}
	parents   []string
}

// NewIndex builds the exact-path set and the deduplicated list of immediate
// parent directories to watch.
func NewIndex(paths []string) *Index {
	ix := &Index{
		sources:   make(map[string]struct{}, len(paths)),
		parentSet: make(map[string]struct{}),
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
	parent := filepath.Dir(clean)
	if _, ok := ix.parentSet[parent]; !ok {
		ix.parentSet[parent] = struct{}{}
		ix.parents = append(ix.parents, parent)
		sort.Strings(ix.parents)
	}
	return true
}

// Match reports whether an event path is one of the selected sources,
// returning its cleaned form.
func (ix *Index) Match(eventPath string) (string, bool) {
	clean := filepath.Clean(eventPath)
	_, ok := ix.sources[clean]
	return clean, ok
}

// Parents returns the distinct immediate parent directories, sorted.
func (ix *Index) Parents() []string { return ix.parents }

// Len returns the number of selected sources.
func (ix *Index) Len() int { return len(ix.sources) }
