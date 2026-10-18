package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// DefaultTrialConfig is where the trial reads its config file when
// TRIAL_CONFIG does not say otherwise.
const DefaultTrialConfig = "/etc/kavach/trial.json"

const minInviteCode = 8

// TrialFile is the trial's config file: what the operator changes while the
// server runs. It holds no secret; keys stay in the environment file. A field
// left empty falls back to the TRIAL_* variable of the same name, and a
// missing file means the variables alone.
type TrialFile struct {
	Invites     []TrialInvite `json:"invites"`
	Tasks       []string      `json:"tasks,omitempty"`
	Models      []string      `json:"models,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
	Showcase    []string      `json:"showcase,omitempty"`
}

// TrialInvite is one invite code, which runs on the server's keys.
type TrialInvite struct {
	Code    string    `json:"code"`
	Label   string    `json:"label,omitempty"` // who it was given to
	Runs    int       `json:"runs"`            // its own budget
	Created time.Time `json:"created"`
	Revoked bool      `json:"revoked,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// LoadTrialFile reads and checks the file at path. A file that does not
// exist is an error satisfying errors.Is(err, fs.ErrNotExist).
func LoadTrialFile(path string) (*TrialFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f TrialFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a misspelt field must not silently do nothing
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Validate reports the first thing wrong with f.
func (f *TrialFile) Validate() error {
	seen := map[string]bool{}
	for _, inv := range f.Invites {
		switch {
		case len(inv.Code) < minInviteCode || strings.ContainsFunc(inv.Code, func(r rune) bool { return r <= ' ' || r > '~' || r == ',' }):
			return fmt.Errorf("invite %q: a code is at least %d visible characters, with no spaces or commas", inv.Label, minInviteCode)
		case seen[inv.Code]:
			return fmt.Errorf("invite %q: its code is listed twice", inv.Label)
		case inv.Runs < 0:
			return fmt.Errorf("invite %q: runs is negative", inv.Label)
		}
		seen[inv.Code] = true
	}
	for _, list := range [][]string{f.Tasks, f.Models, f.Showcase} {
		if slices.Contains(list, "") {
			return errors.New("an empty entry in tasks, models or showcase")
		}
	}
	for _, m := range f.Models {
		if !strings.Contains(m, "/") {
			return fmt.Errorf("model %q is not provider/name", m)
		}
	}
	if f.MaxAttempts < 0 {
		return errors.New("max_attempts is negative")
	}
	return nil
}

// UpdateTrialFile applies edit to the file at path (an empty one if it does
// not exist yet) and writes the result back atomically. The file keeps its
// mode and owner; a new one is 0644, readable by the service user, and holds
// no secret. edit's error, or an invalid result, leaves the file as it was.
func UpdateTrialFile(path string, edit func(*TrialFile) error) error {
	f, err := LoadTrialFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = &TrialFile{}, nil
	}
	if err != nil {
		return err
	}
	if err := edit(f); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Invites == nil {
		f.Invites = []TrialInvite{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o644)
}

// writeFileAtomic writes b beside path and renames it over path. An existing
// file's mode and owner carry over; perm is for a new one.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	old, statErr := os.Stat(path)
	if statErr == nil {
		perm = old.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if statErr == nil && err == nil {
		if st, ok := old.Sys().(*syscall.Stat_t); ok {
			err = tmp.Chown(int(st.Uid), int(st.Gid))
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// InviteDigest is how the usage file names a code, so it holds no code.
func InviteDigest(code string) string { return digest(code) }

// InviteUse is what one invite code has run.
type InviteUse struct {
	Count int       `json:"runs"`
	Last  time.Time `json:"last,omitzero"`
	Jobs  []string  `json:"jobs,omitempty"` // the runs it started, oldest first
}

func (u *InviteUse) runs() int {
	if u == nil {
		return 0
	}
	return u.Count
}

func inviteUseFile(jobs string) string { return filepath.Join(jobs, ".bench", "invites-used.json") }

// ReadInviteUse reads the usage of every invite code, by InviteDigest. Counts
// kept before codes had their own file (trial-invites.json) carry over.
func ReadInviteUse(jobs string) map[string]*InviteUse {
	use := map[string]*InviteUse{}
	if b, err := os.ReadFile(inviteUseFile(jobs)); err == nil {
		json.Unmarshal(b, &use)
		return use
	}
	var legacy map[string]int
	if b, err := os.ReadFile(filepath.Join(jobs, ".bench", "trial-invites.json")); err == nil {
		json.Unmarshal(b, &legacy)
	}
	for k, n := range legacy {
		use[k] = &InviteUse{Count: n}
	}
	return use
}

func saveInviteUse(jobs string, use map[string]*InviteUse) error {
	b, err := json.Marshal(use)
	if err != nil {
		return err
	}
	return writeFileAtomic(inviteUseFile(jobs), b, 0o600)
}

// trialSettings is the part of the configuration that can change while the
// server runs, as the file and the environment together give it.
type trialSettings struct {
	invites     []TrialInvite
	models      []string
	tasks       []string
	maxAttempts int
	showcase    map[string]bool
}

// invite finds the invite whose code is code. Every code is compared, so the
// time taken does not say which one was close.
func (s *trialSettings) invite(code string) (TrialInvite, bool) {
	var found TrialInvite
	ok := false
	for _, inv := range s.invites {
		if subtle.ConstantTimeCompare([]byte(inv.Code), []byte(code)) == 1 {
			found, ok = inv, true
		}
	}
	return found, ok
}

// build combines f, which may be nil, with the environment's values. A code
// in both is the file's, so the file can revoke a code the environment lists.
func (t *trial) build(f *TrialFile) *trialSettings {
	if f == nil {
		f = &TrialFile{}
	}
	s := &trialSettings{
		invites:     slices.Clone(f.Invites),
		models:      nonEmpty(f.Models, t.cfg.Models),
		tasks:       nonEmpty(f.Tasks, t.cfg.Tasks),
		maxAttempts: t.cfg.MaxAttempts,
		showcase:    map[string]bool{},
	}
	if f.MaxAttempts > 0 {
		s.maxAttempts = f.MaxAttempts
	}
	for _, c := range t.cfg.InviteCodes {
		if !slices.ContainsFunc(s.invites, func(i TrialInvite) bool { return i.Code == c }) {
			s.invites = append(s.invites, TrialInvite{Code: c, Runs: t.cfg.InviteRuns})
		}
	}
	for _, id := range nonEmpty(f.Showcase, t.cfg.Showcase) {
		s.showcase[id] = true
	}
	return s
}

func nonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// fileStamp identifies a version of the config file.
type fileStamp struct {
	known bool
	mod   time.Time
	size  int64
	err   string
}

// settings returns the current configuration, reading the config file again
// if it changed since the last call. A file that cannot be read or fails its
// checks is logged once and the last good configuration stays: a typo must
// not open anything up. A file that was removed means the environment alone.
func (t *trial) settings() *trialSettings {
	t.confMu.Lock()
	defer t.confMu.Unlock()
	path := t.cfg.ConfigPath
	if path == "" {
		return t.conf
	}
	stamp := fileStamp{known: true}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		stamp.err = err.Error()
	default:
		stamp.mod, stamp.size = fi.ModTime(), fi.Size()
	}
	if stamp == t.confStamp {
		return t.conf
	}
	t.confStamp = stamp
	switch {
	case stamp.err != "":
		log.Printf("trial config %s: %s; keeping the last good one", path, stamp.err)
	case stamp.mod.IsZero():
		t.conf = t.build(nil)
	default:
		if f, err := LoadTrialFile(path); err != nil {
			log.Printf("trial config: %v; keeping the last good one", err)
		} else {
			t.conf = t.build(f)
			log.Printf("trial config %s loaded: %d invites, %d tasks", path, len(f.Invites), len(f.Tasks))
		}
	}
	return t.conf
}

// ModelKeyVar is the variable a visitor's or the server's key for model
// travels in, and whether the model's provider is one the trial knows.
func ModelKeyVar(model string) (string, bool) {
	v, ok := modelKeyVars[strings.SplitN(model, "/", 2)[0]]
	return v, ok
}
