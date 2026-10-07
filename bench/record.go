package bench

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/kavachlabs/kavach"
)

// Record runs b's events through the planted-bug handler under a flight
// recorder with a stepped clock and seeded randomness, and returns the path of
// the fixture written when the last event fails. Recording is reproducible.
func Record(b Bug, dir string) (string, error) { return RecordWith(b, dir, false) }

// RecordWith is Record with the PII scrubber optionally disabled.
func RecordWith(b Bug, dir string, noScrub bool) (string, error) {
	now := b.Start
	if now.IsZero() {
		now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rec := kavach.NewRecorder(b.New(Buggy), kavach.Options{
		Service:       "bench-" + b.Name,
		Dir:           dir,
		Clock:         func() time.Time { t := now; now = now.Add(90 * time.Minute); return t },
		Rand:          rand.New(rand.NewSource(1)),
		RecoverPanics: true,
		NoScrub:       noScrub,
	})
	var path string
	for i, ev := range b.Events {
		err := rec.Step(kavach.Input{Source: "bench", Position: fmt.Sprint(i), Data: []byte(ev)})
		last := i == len(b.Events)-1
		var pe *kavach.PanicError
		var ie *kavach.InvariantError
		switch {
		case err == nil && last:
			return "", errors.New("the last event did not fail the buggy build")
		case err == nil:
		case !last:
			return "", fmt.Errorf("event %d failed before the incident: %w", i, err)
		case errors.As(err, &pe):
			path = pe.Fixture
		case errors.As(err, &ie):
			path = ie.Fixture
		default:
			// A returned error is flushed by the recorder; find the fixture.
			return findFixture(dir)
		}
	}
	return path, nil
}

func findFixture(dir string) (string, error) {
	m, err := filepath.Glob(filepath.Join(dir, "*.kavach"))
	if err != nil || len(m) != 1 {
		return "", fmt.Errorf("want one fixture in %s, got %v (%v)", dir, m, err)
	}
	return m[0], nil
}
