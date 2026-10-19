package harbor

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// maxEventBytes is the most of the event log one Events call returns.
const maxEventBytes = 256 << 10

// Events is a piece of an attempt's OpenCode event log: whole JSON lines,
// as OpenCode wrote them.
type Events struct {
	// Source is the file read: agent/opencode.txt once Harbor has it, else
	// its live copy agent/opencode-live.txt; "" when there is neither.
	Source string `json:"source"`
	// From and Next are byte offsets in Source: Lines start at From, and
	// the next call passes Next to get what follows.
	From int64 `json:"from"`
	Next int64 `json:"next"`
	// Size is Source's size; Next < Size means there is more to read.
	Size  int64    `json:"size"`
	Lines []string `json:"lines"`
	// Skipped is true when the lines start after the log's beginning,
	// because a call without from tails the log.
	Skipped bool `json:"skipped,omitempty"`
}

// Events reads attempt id's event log from byte from, or, when from is
// negative or past the end (the log was replaced), its last maxEventBytes,
// starting at a line. A line still being written is left for the next call.
func (r *Reader) Events(job, id string, from int64) (Events, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return Events{}, err
	}
	return readEvents(dir, from)
}

func readEvents(dir string, from int64) (Events, error) {
	ev := Events{Lines: []string{}}
	var f *os.File
	for _, name := range []string{"agent/opencode.txt", "agent/opencode-live.txt"} {
		var err error
		if f, err = os.Open(filepath.Join(dir, name)); err == nil {
			ev.Source = name
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return ev, err
		}
	}
	if f == nil {
		return ev, nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ev, err
	}
	ev.Size = st.Size()
	tail := from < 0 || from > ev.Size
	if tail {
		from = max(0, ev.Size-maxEventBytes)
	}
	buf := make([]byte, min(ev.Size-from, maxEventBytes))
	n, err := f.ReadAt(buf, from)
	if err != nil && err != io.EOF {
		return ev, err
	}
	buf = buf[:n]
	if tail && from > 0 {
		// Start at a line: drop the part of the one the tail cut into.
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			buf = nil
		} else {
			buf, from = buf[i+1:], from+int64(i+1)
		}
		ev.Skipped = true
	}
	ev.From = from
	end := bytes.LastIndexByte(buf, '\n') + 1
	if end == 0 && int64(len(buf)) == maxEventBytes {
		// One line longer than a whole read: pass it as it is.
		end = len(buf)
	}
	for _, line := range bytes.Split(buf[:end], []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			ev.Lines = append(ev.Lines, string(line))
		}
	}
	ev.Next = from + int64(end)
	return ev, nil
}
