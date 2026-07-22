package watcher

import (
	"sync"
	"time"
)

// Debouncer coalesces event bursts per source path. Each Trigger arms or
// resets that path's one-shot timer; only after the interval passes with no
// further events does the path appear on C. Timers exist only after an
// event, so an idle debouncer holds no timers and wakes nothing.
type Debouncer struct {
	interval time.Duration
	C        chan string

	mu      sync.Mutex
	timers  map[string]*timerEntry
	done    chan struct{}
	stopped bool
}

type timerEntry struct {
	timer *time.Timer
}

func NewDebouncer(interval time.Duration) *Debouncer {
	return &Debouncer{
		interval: interval,
		C:        make(chan string, 64),
		timers:   make(map[string]*timerEntry),
		done:     make(chan struct{}),
	}
}

// Trigger records an event for path, replacing any pending timer with a fresh
// entry. A queued expiry for a replaced entry is detected in fire by pointer
// identity.
func (d *Debouncer) Trigger(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	if current, ok := d.timers[path]; ok {
		current.timer.Stop()
	}
	entry := &timerEntry{}
	entry.timer = time.AfterFunc(d.interval, func() { d.fire(path, entry) })
	d.timers[path] = entry
}

// Cancel prevents a pending timer for path from firing. A path already queued
// on C is harmless when the consumer also checks current membership.
func (d *Debouncer) Cancel(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.timers[path]
	if !ok {
		return
	}
	entry.timer.Stop()
	delete(d.timers, path)
}

func (d *Debouncer) fire(path string, entry *timerEntry) {
	d.mu.Lock()
	if d.stopped || d.timers[path] != entry {
		d.mu.Unlock()
		return
	}
	delete(d.timers, path)
	d.mu.Unlock()

	select {
	case d.C <- path:
	case <-d.done:
	}
}

// Stop cancels all pending timers. It is safe to call once.
func (d *Debouncer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	for _, entry := range d.timers {
		entry.timer.Stop()
	}
	clear(d.timers)
	close(d.done)
}
