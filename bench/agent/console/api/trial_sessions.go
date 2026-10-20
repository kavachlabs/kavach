package api

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Sessions outlive a restart (a deploy restarts the server): each is written
// to <jobs>/.bench/sessions.json, keyed by the SHA-256 of its id as the owner
// records are, with its invite as the SHA-256 of the code. Keys are never
// written; a session read back has none, and its visitor enters them again.

// savedSession is a session as sessions.json holds it.
type savedSession struct {
	IP     string    `json:"ip"`
	Last   time.Time `json:"last"`
	Invite string    `json:"invite,omitempty"` // SHA-256 of the code
	Runs   []string  `json:"runs,omitempty"`
}

// saveEvery is how stale a session's last use may be in sessions.json: a
// request that only refreshes it writes the file at most this often.
const saveEvery = 10 * time.Minute

func sessionsFile(jobs string) string { return filepath.Join(jobs, ".bench", "sessions.json") }

// loadSessions reads the sessions a previous server wrote, dropping those
// idle for sessionIdle, and the invite of one whose code is no longer
// configured.
func (t *trial) loadSessions() {
	b, err := os.ReadFile(sessionsFile(t.jobs))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("sessions unreadable, starting without them: %v", err)
		}
		return
	}
	var saved map[string]savedSession
	if err := json.Unmarshal(b, &saved); err != nil {
		log.Printf("sessions unreadable, starting without them: %v", err)
		return
	}
	codes := map[string]string{}
	for _, inv := range t.settings().invites {
		codes[digest(inv.Code)] = inv.Code
	}
	now := time.Now()
	for key, s := range saved {
		if now.Sub(s.Last) > sessionIdle || len(key) != 64 {
			continue
		}
		t.sessions[key] = &session{ip: s.IP, last: s.Last, saved: s.Last, invite: codes[s.Invite], runs: s.Runs}
	}
}

// saveSessionsLocked writes every session but its keys; t.mu is held.
func (t *trial) saveSessionsLocked() {
	out := make(map[string]savedSession, len(t.sessions))
	now := time.Now()
	for key, s := range t.sessions {
		v := savedSession{IP: s.ip, Last: s.last, Runs: s.runs}
		if s.invite != "" {
			v.Invite = digest(s.invite)
		}
		out[key] = v
		s.saved = now
	}
	file := sessionsFile(t.jobs)
	b, err := json.Marshal(out)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(file), 0o755)
	}
	if err == nil {
		err = writeFileAtomic(file, b, 0o600, -1)
	}
	if err != nil {
		log.Printf("saving sessions: %v", err)
	}
}
