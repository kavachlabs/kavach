package harbor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEvents(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "agent"), 0o755)
	if ev, err := readEvents(dir, 0); err != nil || ev.Source != "" || len(ev.Lines) != 0 {
		t.Fatalf("no log: %+v %v", ev, err)
	}
	live := filepath.Join(dir, "agent", "opencode-live.txt")
	os.WriteFile(live, []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":"), 0o644)
	ev, err := readEvents(dir, -1)
	if err != nil || ev.Source != "agent/opencode-live.txt" || strings.Join(ev.Lines, "|") != `{"a":1}|{"b":2}` || ev.Next != 16 || ev.Size != 21 || ev.Skipped {
		t.Fatalf("live: %+v %v", ev, err)
	}
	// The line being written comes once it ends.
	os.WriteFile(live, []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":3}\n"), 0o644)
	if ev, _ = readEvents(dir, ev.Next); strings.Join(ev.Lines, "|") != `{"c":3}` || ev.Next != ev.Size {
		t.Fatalf("next: %+v", ev)
	}
	if ev, _ = readEvents(dir, ev.Next); len(ev.Lines) != 0 || ev.From != ev.Size {
		t.Fatalf("at the end: %+v", ev)
	}
	// Harbor's copy wins; a log larger than one read is tailed from a line.
	var b strings.Builder
	for b.Len() <= maxEventBytes {
		b.WriteString(`{"type":"text","part":{"text":"` + strings.Repeat("x", 100) + `"}}` + "\n")
	}
	os.WriteFile(filepath.Join(dir, "agent", "opencode.txt"), []byte(b.String()), 0o644)
	ev, _ = readEvents(dir, -1)
	if ev.Source != "agent/opencode.txt" || !ev.Skipped || ev.Next != ev.Size || !strings.HasPrefix(ev.Lines[0], `{"type"`) || int64(b.Len())-ev.From > maxEventBytes {
		t.Fatalf("tail: source %s skipped %v from %d next %d size %d", ev.Source, ev.Skipped, ev.From, ev.Next, ev.Size)
	}
	// An offset past the end (the log was replaced) tails it too.
	if ev2, _ := readEvents(dir, ev.Size+10); ev2.From != ev.From {
		t.Fatalf("past the end: %d, want %d", ev2.From, ev.From)
	}
}
