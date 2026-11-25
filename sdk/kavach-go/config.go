// Package kavach is the zero-dependency Go core SDK for Kavach v0.2
// frictionless driver-boundary record and deterministic replay.
package kavach

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kavachlabs/kavach-go/host"
)

// Version is the semantic version reported in KVRING02 Open frames and host ready messages.
const Version = "0.3.0"

// HostCommand is the final CLI argument that activates Replay mode (SPEC §9.1).
const HostCommand = host.HostCommand

// Mode selects how Kavach operates inside a service process.
type Mode uint8

const (
	// ModeAuto (the zero value in Config) auto-detects ModeReplay when
	// KAVACH_HOST=1 or os.Args ends with "kavach-host", ModeRecord when
	// RingPath / KAVACH_RING_PATH is set, and ModeOff otherwise.
	ModeAuto Mode = iota
	// ModeOff disables recording and replay with zero runtime overhead.
	ModeOff
	// ModeRecord captures steps non-blockingly into a KVRING02 ring or stream.
	ModeRecord
	// ModeReplay runs the service as a deterministic kavach-host without touching
	// external DBs, Redis, Kafka, or network sockets.
	ModeReplay
)

// String returns the canonical string name of m.
func (m Mode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeOff:
		return "off"
	case ModeRecord:
		return "record"
	case ModeReplay:
		return "replay"
	default:
		return "unknown"
	}
}

// ScrubMode controls deterministic HMAC-SHA256 PII scrubbing on recorded payloads.
type ScrubMode uint8

const (
	// ScrubOff leaves recorded payloads unmodified.
	ScrubOff ScrubMode = iota
	// ScrubStandard tokenizes common PII fields (email, phone, SSN, card, token/secret/auth)
	// and inline PII patterns deterministically so scrubbed traces still replay identically.
	ScrubStandard
	// ScrubStrict applies ScrubStandard and additionally tokenizes all non-allowlisted
	// JSON string leaves.
	ScrubStrict
)

// String returns the canonical string name of s.
func (s ScrubMode) String() string {
	switch s {
	case ScrubOff:
		return "off"
	case ScrubStandard:
		return "standard"
	case ScrubStrict:
		return "strict"
	default:
		return "unknown"
	}
}

// ParseScrubMode parses a string ("off", "standard", "strict") into a ScrubMode.
func ParseScrubMode(s string) (ScrubMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "none", "false":
		return ScrubOff, true
	case "standard", "default", "true":
		return ScrubStandard, true
	case "strict":
		return ScrubStrict, true
	default:
		return ScrubOff, false
	}
}

// Config configures a Kavach instance passed to Init.
type Config struct {
	// Service names the service in KVRING02 Open frames. Defaults to filepath.Base(os.Args[0]).
	Service string
	// RingPath is the path to the KVRING02 shared-memory ring file (or fallback stream).
	// Also honors the KAVACH_RING_PATH environment variable when empty.
	RingPath string
	// RingCapacity is the circular ring capacity in bytes (power of two >= 65536).
	// Defaults to ring.DefaultCapacity (8 MiB) when 0.
	RingCapacity int
	// PII selects deterministic PII scrubbing for recorded traffic (ScrubOff, ScrubStandard, ScrubStrict).
	// Also honors KAVACH_PII when zero.
	PII ScrubMode
	// PIIKey is the HMAC-SHA256 key used for deterministic leaf tokenization.
	// Defaults to KAVACH_PII_KEY env var or a deterministic per-service key.
	PIIKey []byte
	// InvariantsPath is the path to declarative invariants YAML.
	// Defaults to ".kavach/invariants.yaml" when empty and that file exists.
	InvariantsPath string
	// Mode overrides automatic mode detection (ModeAuto, ModeOff, ModeRecord, ModeReplay).
	Mode Mode
	// ListenAddr is the address used by ServeOrReplay when serving an http.Handler
	// outside Replay mode. Defaults to KAVACH_ADDR or ":8080".
	ListenAddr string
	// Stdin overrides os.Stdin for the kavach-host protocol loop (useful in tests).
	Stdin io.Reader
	// Stdout overrides os.Stdout for the kavach-host protocol loop (useful in tests).
	Stdout io.Writer
	// OnReset is invoked when the replay driver sends a "reset" command between runs.
	OnReset func() error
}

// detectReplayArgs reports whether env or argv indicates kavach-host replay mode.
func detectReplayMode(args []string, getenv func(string) string) bool {
	if v := strings.TrimSpace(getenv("KAVACH_HOST")); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	if len(args) > 1 && args[len(args)-1] == HostCommand {
		return true
	}
	return false
}

func resolveConfig(cfg Config) Config {
	if cfg.Service == "" {
		if len(os.Args) > 0 && os.Args[0] != "" {
			cfg.Service = filepath.Base(os.Args[0])
		} else {
			cfg.Service = "service"
		}
	}
	if cfg.RingPath == "" {
		cfg.RingPath = os.Getenv("KAVACH_RING_PATH")
	}
	if cfg.PII == ScrubOff {
		if envPII := os.Getenv("KAVACH_PII"); envPII != "" {
			if m, ok := ParseScrubMode(envPII); ok {
				cfg.PII = m
			}
		}
	}
	if len(cfg.PIIKey) == 0 {
		if envKey := os.Getenv("KAVACH_PII_KEY"); envKey != "" {
			cfg.PIIKey = []byte(envKey)
		} else {
			cfg.PIIKey = []byte("kavach-pii-v1:" + cfg.Service)
		}
	}
	if cfg.InvariantsPath == "" {
		const defaultInv = ".kavach/invariants.yaml"
		if _, err := os.Stat(defaultInv); err == nil {
			cfg.InvariantsPath = defaultInv
		}
	}
	if cfg.ListenAddr == "" {
		if addr := os.Getenv("KAVACH_ADDR"); addr != "" {
			cfg.ListenAddr = addr
		} else {
			cfg.ListenAddr = ":8080"
		}
	}
	if cfg.Mode == ModeAuto {
		switch {
		case detectReplayMode(os.Args, os.Getenv):
			cfg.Mode = ModeReplay
		case cfg.RingPath != "":
			cfg.Mode = ModeRecord
		default:
			cfg.Mode = ModeOff
		}
	}
	return cfg
}
