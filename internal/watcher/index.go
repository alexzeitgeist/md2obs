// Package watcher provides event-driven, non-recursive watching of the
// immediate parent directories of explicitly selected source files.
package watcher

import (
	"path/filepath"
	"sort"
)

// Index is the exact-path filter for one watch session: only paths selected
// from the database at startup ever match. New files appearing in watched
// directories are ignored by construction.
type Index struct {
	sources map[string]struct{}
	parents []string
}

// NewIndex builds the exact-path set and the deduplicated list of immediate
// parent directories to watch.
func NewIndex(paths []string) *Index {
	ix := &Index{sources: make(map[string]struct{}, len(paths))}
	parentSet := make(map[string]struct{})
	for _, p := range paths {
		clean := filepath.Clean(p)
		ix.sources[clean] = struct{}{}
		parentSet[filepath.Dir(clean)] = struct{}{}
	}
	for parent := range parentSet {
		ix.parents = append(ix.parents, parent)
	}
	sort.Strings(ix.parents)
	return ix
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
