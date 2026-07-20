// Package app implements the md2obs commands on top of the internal
// database, layout, source, and materialize packages.
package app

import (
	"io"
	"time"

	"md2obs/internal/config"
	"md2obs/internal/database"
	"md2obs/internal/layout"
)

// Deps carries everything a command needs. Now is injectable so tests can
// control the snapshot date.
type Deps struct {
	DB     *database.DB
	Config *config.Config
	Layout layout.Layout
	Now    func() time.Time
	Out    io.Writer
	Err    io.Writer
}

const dateFormat = "2006-01-02"

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
