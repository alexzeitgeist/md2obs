package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Info holds the metadata recorded for an observed source revision.
type Info struct {
	ByteSize int64
	MtimeNS  int64
}

// Inspect validates that the canonical path is a regular Markdown file and
// returns its metadata.
func Inspect(canonical string) (Info, error) {
	if !strings.EqualFold(filepath.Ext(canonical), ".md") {
		return Info{}, fmt.Errorf("%s: only .md files are supported", canonical)
	}
	fi, err := os.Stat(canonical)
	if err != nil {
		return Info{}, fmt.Errorf("stat %s: %w", canonical, err)
	}
	if !fi.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s is not a regular file", canonical)
	}
	return Info{ByteSize: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}, nil
}
