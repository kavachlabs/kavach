package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

type devResult struct {
	Service   string   `json:"service"`
	OutDir    string   `json:"out_dir"`
	RingPath  string   `json:"ring_path"`
	Steps     int      `json:"steps"`
	Records   int      `json:"records"`
	Capsules  []string `json:"capsules"`
	ExitCode  int      `json:"exit_code"`
	DurationS float64  `json:"duration_s"`
}

func cmdDev(u ui, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cmdDevWithContext(ctx, u, args, stdout, stderr)
}

func cmdDevWithContext(ctx context.Context, u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("dev", stderr)
	cmdStr := fs.String("cmd", "", "command to run under local recording (e.g. \"go run ./cmd/server\")")
	outDir := fs.String("out", kconfig.DefaultCapsulesDir, "directory to write recorded .kavach capsules")
	service := fs.String("service", "", "override recorded service name")
	pii := fs.String("pii", kconfig.PIIScrubStandard, "PII scrub mode passed to service: standard, strict, or off")
	ringPathFlag := fs.String("ring-path", "", "explicit KVRING02 ring file path (default: temp file)")
	ringSize := fs.Uint64("ring-size", recstream.DefaultRing, "KVRING02 ring capacity in bytes (power of two >= 65536)")
	asJSON := fs.Bool("json", false, "print the recording summary as JSON")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	command := strings.TrimSpace(*cmdStr)
	if command == "" && len(pos) > 0 {
		command = strings.Join(pos, " ")
	}
	if command == "" {
		fmt.Fprintln(stderr, "kavach dev: --cmd \"<command>\" is required")
		return exitUsage
	}
	switch strings.ToLower(*pii) {
	case kconfig.PIIScrubOff, kconfig.PIIScrubStandard, kconfig.PIIScrubStrict:
	default:
		fmt.Fprintf(stderr, "kavach dev: invalid --pii %q (expected standard, strict, or off)\n", *pii)
		return exitUsage
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "kavach dev: create %s: %v\n", *outDir, err)
		return exitError
	}

	// Default service name from .kavach/config.yaml if present.
	svcName := strings.TrimSpace(*service)
	if svcName == "" {
		if cfg, err := kconfig.LoadConfig(kconfig.DefaultConfigFile); err == nil && cfg.Service != "" {
			svcName = cfg.Service
		}
	}

	ringPath := strings.TrimSpace(*ringPathFlag)
	ownsRingFile := false
	if ringPath == "" {
		tmpDir, err := os.MkdirTemp("", "kavach-dev-*")
		if err != nil {
			fmt.Fprintf(stderr, "kavach dev: temp dir: %v\n", err)
			return exitError
		}
		defer os.RemoveAll(tmpDir)
		ringPath = filepath.Join(tmpDir, "kavach.shm")
		ownsRingFile = true
	}

	ring, err := createDevRing(ringPath, *ringSize)
	if err != nil {
		fmt.Fprintf(stderr, "kavach dev: %v\n", err)
		return exitError
	}
	defer func() {
		_ = ring.Close()
		if ownsRingFile {
			_ = os.Remove(ringPath)
		}
	}()

	cmd := buildDevCommand(command)
	cmd.Env = append(os.Environ(),
		"KAVACH_RING_PATH="+ringPath,
		"KAVACH_MODE=record",
		"KAVACH_PII="+strings.ToLower(*pii),
	)
	if svcName != "" {
		cmd.Env = append(cmd.Env, "KAVACH_SERVICE="+svcName)
	}
	if *asJSON {
		cmd.Stdout = stderr
	} else {
		cmd.Stdout = stdout
	}
	cmd.Stderr = stderr

	start := time.Now()
	if !*asJSON {
		u.printBanner(stdout)
		u.header(stdout, "KAVACH DEV RECORDER")
		fmt.Fprintf(stdout, "%s%s\n", u.label("command", 10), command)
		fmt.Fprintf(stdout, "%s%s\n", u.label("ring", 10), ringPath)
		fmt.Fprintf(stdout, "%s%s (pii=%s)\n", u.label("out", 10), *outDir, strings.ToLower(*pii))
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "kavach dev: start command: %v\n", err)
		return exitError
	}

	// Forward Ctrl+C / SIGTERM cleanly to child process so it flushes Close().
	stopSig := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Signal(os.Interrupt)
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				select {
				case <-stopSig:
				case <-timer.C:
					_ = cmd.Process.Kill()
				}
			}
		case <-stopSig:
		}
	}()

	dec := newDevStreamCollector(svcName, *outDir)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	var waitErr error
	var streamFallbackOffset int64
	drainOnce := func() error {
		b, err := ring.PollOnce()
		if err != nil {
			return err
		}
		if len(b) > 0 {
			return dec.Feed(b)
		}
		// Fallback: if the child replaced the ring path with a plain stream file
		// (no KVRING02 magic), tail-read raw recstream frames from offset 0.
		if !dec.sawAnyBytes {
			if raw, nextOff, ok := pollFallbackStreamFile(ringPath, streamFallbackOffset); ok && len(raw) > 0 {
				streamFallbackOffset = nextOff
				return dec.Feed(raw)
			}
		}
		return nil
	}

loop:
	for {
		select {
		case waitErr = <-waitCh:
			close(stopSig)
			break loop
		case <-ticker.C:
			if err := drainOnce(); err != nil {
				fmt.Fprintf(stderr, "kavach dev: drain warning: %v\n", err)
			}
		}
	}

	// Final drain after child exit to catch last frames before Close.
	for i := 0; i < 4; i++ {
		if err := drainOnce(); err != nil {
			fmt.Fprintf(stderr, "kavach dev: final drain warning: %v\n", err)
			break
		}
		if dec.closed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := dec.FinishEOF(); err != nil {
		fmt.Fprintf(stderr, "kavach dev: write capsule: %v\n", err)
		return exitError
	}

	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}

	res := devResult{
		Service:   dec.serviceName(),
		OutDir:    *outDir,
		RingPath:  ringPath,
		Steps:     dec.steps,
		Records:   len(dec.records),
		Capsules:  dec.capsules,
		ExitCode:  exitCode,
		DurationS: time.Since(start).Seconds(),
	}

	if *asJSON {
		return writeJSON(stdout, stderr, res)
	}

	fmt.Fprintf(stdout, "\n%s%d steps · %d records · %.2fs\n", u.label("recorded", 10), res.Steps, res.Records, res.DurationS)
	if len(res.Capsules) == 0 {
		fmt.Fprintf(stdout, "%sno records captured (ensure your service initializes kavach-go with KAVACH_RING_PATH)\n", u.label("capsules", 10))
	} else {
		for _, c := range res.Capsules {
			fmt.Fprintf(stdout, "%s%s\n", u.label("capsule", 10), u.paint(c, ansiGreen))
		}
	}
	return exitPass
}

func buildDevCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command)
	}
	return exec.Command("sh", "-c", command)
}

func pollFallbackStreamFile(path string, offset int64) ([]byte, int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, false
	}
	defer f.Close()
	var magic [8]byte
	if n, _ := f.ReadAt(magic[:], 0); n == 8 && string(magic[:]) == "KVRING02" {
		return nil, offset, false
	}
	st, err := f.Stat()
	if err != nil || st.Size() <= offset {
		return nil, offset, false
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, false
	}
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return nil, offset, false
	}
	return b, offset + int64(len(b)), true
}

// devStreamCollector decodes uvarint-framed recstream packets from a KVRING02
// ring (or stream fallback) and writes valid .kavach journal files.
type devStreamCollector struct {
	serviceOverride string
	outDir          string

	openMeta    recstream.Open
	hasOpen     bool
	pending     []byte
	records     []journal.Record
	envSent     bool
	activeStep  bool
	stepFailed  bool
	closed      bool
	steps       int
	flushedSeq  int // record count at last capsule write
	flushCount  int
	capsules    []string
	sawAnyBytes bool
}

func newDevStreamCollector(serviceOverride, outDir string) *devStreamCollector {
	return &devStreamCollector{
		serviceOverride: serviceOverride,
		outDir:          outDir,
	}
}

func (d *devStreamCollector) serviceName() string {
	if d.serviceOverride != "" {
		return d.serviceOverride
	}
	if d.openMeta.Service != "" {
		return d.openMeta.Service
	}
	return "service"
}

func (d *devStreamCollector) Feed(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	d.sawAnyBytes = true
	d.pending = append(d.pending, chunk...)

	off := 0
	for off < len(d.pending) {
		rem := d.pending[off:]
		flen, hdr := binary.Uvarint(rem)
		if hdr == 0 {
			break // incomplete varint
		}
		if hdr < 0 || flen == 0 || flen > recstream.MaxFrame {
			d.pending = nil
			return fmt.Errorf("invalid recstream frame length %d", flen)
		}
		if uint64(len(rem)-hdr) < flen {
			break // incomplete frame payload
		}
		kind := rem[hdr]
		body := rem[hdr+1 : hdr+int(flen)]
		if err := d.handleFrame(kind, body); err != nil {
			d.pending = d.pending[off+hdr+int(flen):]
			return err
		}
		off += hdr + int(flen)
	}
	if off > 0 {
		d.pending = append(d.pending[:0], d.pending[off:]...)
	}
	return nil
}

func (d *devStreamCollector) ensureGenesisEnv(facts []journal.Fact) {
	if d.envSent {
		return
	}
	if facts == nil {
		facts = []journal.Fact{}
	}
	journal.SortFacts(facts)
	d.appendRecord(journal.Record{
		Type:  journal.TypeEnvironment,
		Flags: journal.FlagCritical,
		Facts: facts,
	})
	d.envSent = true
}

func (d *devStreamCollector) appendRecord(r journal.Record) {
	switch r.Type {
	case journal.TypeGateway, journal.TypeEnvironment, journal.TypeConfig:
		r.Flags |= journal.FlagCritical
	}
	r.Seq = uint64(len(d.records))
	d.records = append(d.records, r)
}

func (d *devStreamCollector) handleFrame(kind byte, body []byte) error {
	switch kind {
	case recstream.KindOpen:
		o, err := recstream.ParseOpen(body)
		if err != nil {
			return err
		}
		d.openMeta = o
		d.hasOpen = true
		return nil

	case recstream.KindFacts:
		facts, err := recstream.ParseFacts(body)
		if err != nil {
			return err
		}
		journal.SortFacts(facts)
		d.appendRecord(journal.Record{
			Type:  journal.TypeEnvironment,
			Flags: journal.FlagCritical,
			Facts: facts,
		})
		d.envSent = true
		return nil

	case recstream.KindSnapshot:
		snap, err := recstream.ParseSnapshot(body)
		if err != nil {
			return err
		}
		if len(d.records) == 0 {
			d.openMeta.Start = journal.StartSnapshot
		}
		d.appendRecord(journal.Record{
			Type: journal.TypeSnapshot,
			Data: snap,
		})
		return nil

	case recstream.KindRecord:
		rec, err := recstream.ParseRecord(body)
		if err != nil {
			return err
		}
		if rec.Type == journal.TypeInput {
			d.ensureGenesisEnv(nil)
			d.activeStep = true
			d.stepFailed = false
			d.steps++
		} else if !d.envSent && rec.Type != journal.TypeSnapshot {
			d.ensureGenesisEnv(nil)
		}
		if rec.Type == journal.TypeMarker {
			switch rec.Kind {
			case journal.MarkerPanic, journal.MarkerError, journal.MarkerInvariant, journal.MarkerCrash:
				d.stepFailed = true
			}
		}
		d.appendRecord(rec)
		return nil

	case recstream.KindStepEnd:
		d.activeStep = false
		if d.stepFailed {
			d.stepFailed = false
			return d.flushCapsule("incident")
		}
		return nil

	case recstream.KindFlush:
		if _, err := recstream.ParseFlush(body); err != nil {
			return err
		}
		if len(d.records) > d.flushedSeq {
			label := "baseline"
			if d.stepFailed {
				label = "incident"
			}
			return d.flushCapsule(label)
		}
		return nil

	case recstream.KindClose:
		d.closed = true
		return nil

	default:
		return fmt.Errorf("unknown recstream frame kind 0x%02x", kind)
	}
}

func (d *devStreamCollector) FinishEOF() error {
	if d.activeStep {
		d.appendRecord(journal.Record{
			Type:    journal.TypeMarker,
			Flags:   journal.FlagCritical,
			Kind:    journal.MarkerCrash,
			Message: "process ended during step",
		})
		d.activeStep = false
		d.stepFailed = true
	}
	if len(d.records) == 0 || len(d.records) == d.flushedSeq {
		return nil
	}
	label := "baseline"
	if d.stepFailed || d.hasAnyFailureMarker() {
		label = "incident"
	}
	return d.flushCapsule(label)
}

func (d *devStreamCollector) hasAnyFailureMarker() bool {
	for _, r := range d.records {
		if r.Type == journal.TypeMarker {
			switch r.Kind {
			case journal.MarkerPanic, journal.MarkerError, journal.MarkerInvariant, journal.MarkerCrash:
				return true
			}
		}
	}
	return false
}

func (d *devStreamCollector) flushCapsule(label string) error {
	if len(d.records) == 0 {
		return nil
	}
	d.flushCount++
	svc := sanitizeCapsuleToken(d.serviceName())
	ts := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("%s-%s-%s-%02d.kavach", svc, label, ts, d.flushCount)
	dest := filepath.Join(d.outDir, name)

	startMode := journal.StartGenesis
	if d.records[0].Type == journal.TypeSnapshot {
		startMode = journal.StartSnapshot
	}
	meta := journal.Meta{
		Service:     d.serviceName(),
		Start:       startMode,
		Compression: journal.CompressionZstd,
		Handler:     d.openMeta.Handler,
		Producer:    d.openMeta.Producer,
		Recorder:    "kavach-dev/" + version,
		RecordedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := journal.WriteFile(dest, meta, d.records); err != nil {
		return err
	}
	d.flushedSeq = len(d.records)
	d.capsules = append(d.capsules, dest)
	return nil
}

func sanitizeCapsuleToken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "service"
	}
	return out
}
