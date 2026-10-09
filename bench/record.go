package bench

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// Record runs b's events through the planted-bug handler under a flight
// recorder with a stepped clock and seeded randomness, and returns the path of
// the fixture the recorder writes when the last event fails. It starts
// kavach-recorder, found as the SDK does.
func Record(b Bug, dir string) (string, error) {
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
	})
	defer rec.Close()
	for i, ev := range b.Events {
		err := rec.Step(kavach.Input{Source: "bench", Position: fmt.Sprint(i), Data: []byte(ev)})
		switch last := i == len(b.Events)-1; {
		case err == nil && last:
			return "", errors.New("the last event did not fail the buggy build")
		case err != nil && !last:
			return "", fmt.Errorf("event %d failed before the incident: %w", i, err)
		}
	}
	if err := rec.Flush(true); err != nil {
		return "", err
	}
	return findFixture(dir)
}

func findFixture(dir string) (string, error) {
	m, err := filepath.Glob(filepath.Join(dir, "fixtures", "*.kavach"))
	if err != nil || len(m) != 1 {
		return "", fmt.Errorf("want one fixture in %s, got %v (%v)", dir, m, err)
	}
	return m[0], nil
}
