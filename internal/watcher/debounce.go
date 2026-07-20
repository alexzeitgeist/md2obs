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
	timer      *time.Timer
	generation uint64
}

func NewDebouncer(interval time.Duration) *Debouncer {
	return &Debouncer{
		interval: interval,
		C:        make(chan string, 64),
		timers:   make(map[string]*timerEntry),
		done:     make(chan struct{}),
	}
}

// Trigger records an event for path, starting or resetting its timer.
func (d *Debouncer) Trigger(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	entry, ok := d.timers[path]
	if !ok {
		entry = &timerEntry{}
		d.timers[path] = entry
	} else {
		entry.timer.Stop()
	}
	entry.generation++
	generation := entry.generation
	entry.timer = time.AfterFunc(d.interval, func() { d.fire(path, entry, generation) })
}

func (d *Debouncer) fire(path string, entry *timerEntry, generation uint64) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	current, ok := d.timers[path]
	if !ok || current != entry || current.generation != generation {
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
