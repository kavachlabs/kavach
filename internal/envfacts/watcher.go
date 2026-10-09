package envfacts

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// Watcher polls the `host.` facts and remembers which keys changed, so that a
// recorder can write a change record at the next step boundary (SPEC.md §4.8).
// It never blocks the caller: Take only swaps a map.
type Watcher struct {
	c        *Collector
	interval time.Duration

	mu      sync.Mutex
	last    map[string]journal.Fact
	pending map[string]journal.Fact

	stop chan struct{}
	done chan struct{}
}

// NewWatcher returns a Watcher that compares polls of c against baseline, the
// facts the journal already holds. Only its `host.` facts are watched.
func NewWatcher(c *Collector, interval time.Duration, baseline []journal.Fact) *Watcher {
	w := &Watcher{c: c, interval: interval, last: map[string]journal.Fact{}, pending: map[string]journal.Fact{}}
	for _, f := range baseline {
		if strings.HasPrefix(f.Key, "host.") {
			w.last[f.Key] = f
		}
	}
	return w
}

// Start polls every interval in a goroutine until Stop.
func (w *Watcher) Start() {
	w.stop, w.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(w.done)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				w.Poll()
			}
		}
	}()
}

// Stop ends polling and waits for the goroutine.
func (w *Watcher) Stop() {
	if w.stop == nil {
		return
	}
	close(w.stop)
	<-w.done
	w.stop = nil
}

// Poll collects the facts once and records what differs from the last poll.
func (w *Watcher) Poll() {
	now := map[string]journal.Fact{}
	for _, f := range w.c.Host() {
		now[f.Key] = f
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, f := range now {
		if old, ok := w.last[k]; !ok || old.Form != f.Form || !bytes.Equal(old.Value, f.Value) {
			w.pending[k] = f
		}
	}
	for k := range w.last {
		if _, ok := now[k]; !ok {
			w.pending[k] = journal.Fact{Key: k, Form: journal.FactUnset}
		}
	}
	w.last = now
}

// Take returns the facts that changed since the last call, sorted by key, and
// forgets them. A fact that disappeared has form FactUnset.
func (w *Watcher) Take() []journal.Fact {
	w.mu.Lock()
	p := w.pending
	w.pending = map[string]journal.Fact{}
	w.mu.Unlock()
	if len(p) == 0 {
		return nil
	}
	out := make([]journal.Fact, 0, len(p))
	for _, f := range p {
		out = append(out, f)
	}
	journal.SortFacts(out)
	return out
}
