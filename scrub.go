package kavach

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"github.com/kavachlabs/kavach/journal"
)

// ScrubVersion is the version of the scrubbing rules, recorded in Meta.Scrub.
const ScrubVersion = 1

// RedactedValue replaces the value of an environment variable whose name marks
// it as a secret.
const RedactedValue = "[REDACTED]"

var secretName = regexp.MustCompile(`(?i)PASSWORD|PASSWD|SECRET|TOKEN|KEY|CREDENTIAL|AUTH|COOKIE|SESSION`)

var (
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)
	reJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}`)
	reToken  = regexp.MustCompile(`\b(?:[sprk]{2}_(?:live|test)_[A-Za-z0-9]{8,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9\-]{10,}|sk-[A-Za-z0-9]{20,})`)
	reTokPfx = regexp.MustCompile(`^(?:[a-z]{2}_(?:live|test)_|AKIA|gh[pousr]_|xox[baprs]-|sk-|eyJ)`)
	reCard   = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)
	reIPv4   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	rePhone  = regexp.MustCompile(`\+\d{1,3}[\d\-. ()]{7,14}\d|\b\(?\d{3}\)?[\-. ]\d{3}[\-. ]\d{4}\b`)
)

// Scrubber replaces personal data with stable, shape-preserving placeholders.
//
// The same value always maps to the same placeholder within one Scrubber, so
// equality between fields survives (two inputs for the same customer still
// match). The mapping uses a random per-Scrubber salt that is never stored, so
// a placeholder cannot be reversed or linked across fixtures. Placeholders keep
// the format of what they replace: an email stays an email, a card number stays
// Luhn-valid and the same length, an IP address stays an IP address.
type Scrubber struct {
	salt   []byte
	Counts map[string]int

	learned map[string]string // email local part -> placeholder
	frag    *regexp.Regexp
	dirty   bool
	learn   bool // first pass of Journal: collect, do not use fragments
}

// NewScrubber returns a Scrubber with a fresh random salt.
func NewScrubber() *Scrubber {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return &Scrubber{salt: salt, Counts: map[string]int{}, learned: map[string]string{}}
}

type span struct {
	start, end int
	kind       string
	prio       int
	repl       []byte
}

// Bytes returns b with personal data replaced. b is not modified.
func (s *Scrubber) Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var spans []span
	add := func(re *regexp.Regexp, kind string, prio int, keep func([]byte) bool, mk func(m []byte) []byte) {
		for _, loc := range re.FindAllIndex(b, -1) {
			m := b[loc[0]:loc[1]]
			if keep != nil && !keep(m) {
				continue
			}
			spans = append(spans, span{loc[0], loc[1], kind, prio, mk(m)})
		}
	}
	add(reJWT, "token", 0, nil, s.token)
	add(reToken, "token", 0, nil, s.token)
	add(reEmail, "email", 1, func(m []byte) bool { return !strings.HasSuffix(string(m), "@example.invalid") }, s.email)
	add(reCard, "card", 2, func(m []byte) bool { return luhnOK(m) && cardPrefixOK(m) }, s.card)
	add(reIPv4, "ip", 3, publicIPv4, s.ip)
	add(rePhone, "phone", 4, nil, s.phone)
	if !s.learn && len(s.learned) > 0 {
		for _, loc := range s.fragments().FindAllIndex(b, -1) {
			m := b[loc[0]:loc[1]]
			spans = append(spans, span{loc[0], loc[1], "name", 5, []byte(s.learned[string(m)])})
		}
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].prio < spans[j].prio
	})
	var out []byte
	pos := 0
	for _, sp := range spans {
		if sp.start < pos {
			continue // overlaps a higher-priority match
		}
		out = append(out, b[pos:sp.start]...)
		out = append(out, sp.repl...)
		pos = sp.end
		s.Counts[sp.kind]++
	}
	if pos == 0 && out == nil {
		return b
	}
	return append(out, b[pos:]...)
}

// String is Bytes for strings.
func (s *Scrubber) String(v string) string { return string(s.Bytes([]byte(v))) }

// stream returns n deterministic pseudo-random bytes for (kind, value).
func (s *Scrubber) stream(kind string, v []byte, n int) []byte {
	var out []byte
	for ctr := uint32(0); len(out) < n; ctr++ {
		h := hmac.New(sha256.New, s.salt)
		h.Write([]byte(kind))
		h.Write([]byte{0})
		h.Write(v)
		h.Write(binary.LittleEndian.AppendUint32(nil, ctr))
		out = h.Sum(out)
	}
	return out[:n]
}

var rePlaceholderLocal = regexp.MustCompile(`^user-[0-9a-f]{8}$`)

// email replaces the local part of an address and keeps the domain: handlers
// often branch on or emit the domain, and a domain names an organisation, not
// a person. The replaced local part is remembered, so that a handler which
// emits it on its own (split on "@", say) still matches the scrubbed input.
func (s *Scrubber) email(m []byte) []byte {
	at := bytes.LastIndexByte(m, '@')
	local := m[:at]
	if rePlaceholderLocal.Match(local) {
		return m
	}
	repl := "user-" + hex.EncodeToString(s.stream("email", m, 4))
	if len(local) >= 3 {
		if _, ok := s.learned[string(local)]; !ok {
			s.learned[string(local)] = repl
			s.dirty = true
		}
	}
	return append([]byte(repl), m[at:]...)
}

// fragments returns a matcher for the local parts learned so far.
func (s *Scrubber) fragments() *regexp.Regexp {
	if s.dirty {
		names := make([]string, 0, len(s.learned))
		for n := range s.learned {
			names = append(names, regexp.QuoteMeta(n))
		}
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		s.frag = regexp.MustCompile(`\b(?:` + strings.Join(names, "|") + `)\b`)
		s.dirty = false
	}
	return s.frag
}

func (s *Scrubber) token(m []byte) []byte {
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	keep := len(reTokPfx.Find(m))
	out := append([]byte(nil), m...)
	st := s.stream("token", m, len(m))
	for i := keep; i < len(out); i++ {
		if out[i] == '.' || out[i] == '-' || out[i] == '_' {
			continue // keep JWT and token structure
		}
		out[i] = alnum[int(st[i])%len(alnum)]
	}
	return out
}

// digits replaces every digit of m, keeping separators.
func (s *Scrubber) digits(kind string, m []byte, skip int) []byte {
	out := append([]byte(nil), m...)
	st := s.stream(kind, m, len(m))
	seen := 0
	for i, c := range out {
		if c < '0' || c > '9' {
			continue
		}
		if seen++; seen > skip {
			out[i] = '0' + st[i]%10
		}
	}
	return out
}

func (s *Scrubber) phone(m []byte) []byte {
	skip := 0
	if m[0] == '+' {
		skip = 1 // keep the first digit of the country code
	}
	return s.digits("phone", m, skip)
}

func (s *Scrubber) card(m []byte) []byte {
	out := s.digits("card", m, 1) // keep the network digit
	// Make the number Luhn-valid again by fixing the last digit.
	last := -1
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] >= '0' && out[i] <= '9' {
			last = i
			break
		}
	}
	for d := byte('0'); d <= '9'; d++ {
		out[last] = d
		if luhnOK(out) {
			break
		}
	}
	return out
}

func (s *Scrubber) ip(m []byte) []byte {
	st := s.stream("ip", m, 3)
	return []byte("10." + itoa3(st[0]) + "." + itoa3(st[1]) + "." + itoa3(st[2]))
}

func itoa3(b byte) string {
	const d = "0123456789"
	if b >= 100 {
		return string([]byte{d[b/100], d[b/10%10], d[b%10]})
	}
	if b >= 10 {
		return string([]byte{d[b/10], d[b%10]})
	}
	return string([]byte{d[b]})
}

func luhnOK(m []byte) bool {
	sum, n, alt := 0, 0, false
	for i := len(m) - 1; i >= 0; i-- {
		c := m[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
		n++
	}
	return n >= 13 && n <= 19 && sum%10 == 0
}

var reIIN = regexp.MustCompile(`^(?:4(?:\d{12}|\d{15}|\d{18})|5[1-5]\d{14}|2(?:2[2-9]|[3-6]\d|7[01]|720)\d{13}|3[47]\d{13}|35\d{14,17}|6011\d{12}|65\d{14})$`)

// cardPrefixOK limits unseparated digit runs to real card number ranges, so
// that timestamps and ids that happen to pass the Luhn check are left alone.
// Groups separated by spaces or dashes are accepted on the Luhn check alone.
func cardPrefixOK(m []byte) bool {
	if bytes.ContainsAny(m, " -") {
		return true
	}
	return reIIN.Match(m)
}

func publicIPv4(m []byte) bool {
	parts := strings.Split(string(m), ".")
	for _, p := range parts {
		if len(p) > 3 || (len(p) > 1 && p[0] == '0') || atoi(p) > 255 {
			return false
		}
	}
	return !(parts[0] == "127" || string(m) == "0.0.0.0")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// Journal returns meta and records with personal data replaced and
// meta.Scrub / meta.Env.Scrubbed set. The inputs are not modified. Clock and
// rand records are left alone: they are not personal and replay depends on
// them. Input sources and positions, output data, marker messages and data,
// snapshots, the variant description and environment variables are scrubbed.
func (s *Scrubber) Journal(meta journal.Meta, recs []journal.Record) (journal.Meta, []journal.Record) {
	// Two passes: the first learns the local parts of every email in the
	// journal, the second replaces them wherever they appear on their own.
	s.learn = true
	s.records(recs)
	s.learn = false
	s.Counts = map[string]int{}
	out := s.records(recs)
	return s.header(meta, out)
}

func (s *Scrubber) records(recs []journal.Record) []journal.Record {
	out := make([]journal.Record, len(recs))
	for i, r := range recs {
		switch r.Type {
		case journal.TypeInput:
			r.Source, r.Position, r.Data = s.String(r.Source), s.String(r.Position), s.Bytes(r.Data)
		case journal.TypeOutput, journal.TypeSnapshot:
			r.Data = s.Bytes(r.Data)
		case journal.TypeMarker:
			r.Message, r.Data = s.String(r.Message), s.Bytes(r.Data)
		}
		out[i] = r
	}
	return out
}

func (s *Scrubber) header(meta journal.Meta, out []journal.Record) (journal.Meta, []journal.Record) {
	if v := meta.Variant; v != nil {
		c := *v
		c.Failure = s.String(c.Failure)
		meta.Variant = &c
	}
	if e := meta.Env; e != nil {
		c := *e
		c.Vars = make(map[string]string, len(e.Vars))
		for name, val := range e.Vars {
			if secretName.MatchString(name) && val != "" {
				c.Vars[name] = RedactedValue
				s.Counts["env_secret"]++
			} else {
				c.Vars[name] = s.String(val)
			}
		}
		if e.System != nil {
			sys := *e.System
			if sys.Hostname != "" {
				sys.Hostname = "host-" + hex.EncodeToString(s.stream("hostname", []byte(sys.Hostname), 3))
				s.Counts["hostname"]++
			}
			c.System = &sys
		}
		c.Scrubbed = true
		meta.Env = &c
	}
	counts := make(map[string]int, len(s.Counts))
	for k, v := range s.Counts {
		counts[k] = v
	}
	meta.Scrub = &journal.Scrub{Version: ScrubVersion, Redactions: counts}
	return meta, out
}
