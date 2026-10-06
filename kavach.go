package kavach

import (
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// Version is the version of this library, recorded in fixture headers.
const Version = "0.1.0-dev"

// Input is one event consumed by a handler.
type Input struct {
	Source   string // where the event came from, e.g. "kafka:wallet-events"
	Position string // its position in that source, e.g. "3:1042"
	Data     []byte // the event exactly as received
}

// Output is one effect a handler requested.
type Output struct {
	Sink string `json:"sink"`
	Data []byte `json:"data"`
}

// Clock is the source of time a handler must use instead of time.Now.
type Clock interface {
	Now() time.Time
}

// Rand is the source of randomness a handler must use instead of math/rand or
// crypto/rand. Read always fills p completely.
type Rand interface {
	Read(p []byte) (int, error)
}

// Env is passed to a handler for each input. Everything nondeterministic a
// handler does must go through it: reading time, reading randomness, and
// producing effects. When recording, reads are journaled; when replaying, they
// are served from the journal and effects are captured instead of executed.
type Env interface {
	Clock
	Rand
	// Emit requests an effect. Effects are delivered only after the step
	// returns successfully, and never during replay.
	Emit(sink string, data []byte)
}

// Handler folds inputs into state. It must be deterministic given the Env:
// the same state, input, clock and random values must produce the same outputs.
// Handlers are called from one goroutine at a time.
type Handler interface {
	Handle(env Env, in Input) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(env Env, in Input) error

// Handle calls f.
func (f HandlerFunc) Handle(env Env, in Input) error { return f(env, in) }

// Snapshotter is implemented by handlers whose state can be saved and restored.
// It lets the flight recorder keep a bounded window of history: a fixture then
// starts from a snapshot instead of from the first input the service ever saw.
type Snapshotter interface {
	Snapshot() ([]byte, error)
	Restore(data []byte) error
}

// Invariant is a named property of handler state that must hold after every step.
type Invariant struct {
	Name  string
	Check func() error
}

// Checker is implemented by handlers that declare invariants.
type Checker interface {
	Invariants() []Invariant
}

// Journal is an append-only sequence of records. journal.File implements it.
type Journal interface {
	Append(r journal.Record) (uint64, error)
	Iterate(fn func(journal.Record) error) error
}

var _ Journal = (*journal.File)(nil)

func checkInvariants(h Handler) (name string, err error) {
	c, ok := h.(Checker)
	if !ok {
		return "", nil
	}
	for _, inv := range c.Invariants() {
		if err := inv.Check(); err != nil {
			return inv.Name, err
		}
	}
	return "", nil
}
