// Package app implements the md2obs commands on top of the internal
// database, layout, source, and materialize packages.
package app

import (
	"io"
	"log/slog"
	"sync"
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
	Log    *slog.Logger

	logOnce sync.Once
}

func (d *Deps) logger() *slog.Logger {
	d.logOnce.Do(func() {
		if d.Log != nil {
			return
		}
		w := d.Err
		if w == nil {
			w = io.Discard
		}
		d.Log = slog.New(slog.NewTextHandler(w, nil))
	})
	return d.Log
}

const dateFormat = "2006-01-02"

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
