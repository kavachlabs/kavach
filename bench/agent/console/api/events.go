package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// event is one Server-Sent Event: "job" or "trial", with its JSON.
type event struct {
	name string
	data []byte
}

// hub polls the jobs folder while anyone listens and sends each job and
// trial that changed.
type hub struct {
	reader   *harbor.Reader
	interval time.Duration

	mu     sync.Mutex
	subs   map[chan event]bool
	cancel context.CancelFunc
}

// subBuffer bounds the events queued for one listener; a listener that falls
// further behind is dropped and must reconnect.
const subBuffer = 1024

func (h *hub) subscribe() chan event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan event]bool{}
	}
	ch := make(chan event, subBuffer)
	h.subs[ch] = true
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.poll(ctx)
	}
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[ch] {
		delete(h.subs, ch)
		close(ch)
	}
	if len(h.subs) == 0 && h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

func (h *hub) poll(ctx context.Context) {
	prev := h.snapshot(nil)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prev = h.snapshot(prev)
		}
	}
}

// snapshot reads every job and sends what differs from prev, keyed by job
// or trial; with prev nil it only records the baseline.
func (h *hub) snapshot(prev map[string][]byte) map[string][]byte {
	jobs, err := h.reader.Jobs()
	if err != nil {
		return prev
	}
	next := map[string][]byte{}
	var changed []event
	note := func(key, name string, v any) {
		b, _ := json.Marshal(v)
		next[key] = b
		if prev != nil && !bytes.Equal(prev[key], b) {
			changed = append(changed, event{name, b})
		}
	}
	for _, j := range jobs {
		trials := j.Trials
		j.Trials = nil
		note("job/"+j.ID, "job", j)
		for _, t := range trials {
			if t.ID != "" {
				note("trial/"+j.ID+"/"+t.ID, "trial", t)
			}
		}
	}
	if len(changed) > 0 {
		h.mu.Lock()
	subs:
		for ch := range h.subs {
			for _, e := range changed {
				select {
				case ch <- e:
				default:
					delete(h.subs, ch)
					close(ch)
					continue subs
				}
			}
		}
		h.mu.Unlock()
	}
	return next
}

// pingEvery keeps idle connections from being closed by proxies and lets a
// client notice a dead server.
const pingEvery = 15 * time.Second

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Every job as it is now, so a client has no gap between its GET and
	// the first change.
	jobs, _ := s.reader.Jobs()
	for _, j := range jobs {
		j.Trials = nil
		b, _ := json.Marshal(j)
		writeEvent(w, event{"job", b})
	}
	flusher.Flush()

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(w, e)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e event) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
}
