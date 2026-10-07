package kavach_test

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
)

func TestScrubDetectors(t *testing.T) {
	// Built at run time so that no secret-shaped literal sits in the source.
	stripeLike := "sk_" + "live_" + strings.Repeat("aB3dE", 5)
	in := `{"email":"Ada.Lovelace+x@analytical.co.uk","phone":"+44 20 7946 0958","us":"415-555-0132",` +
		`"card":"4111 1111 1111 1111","ip":"203.0.113.42","lo":"127.0.0.1","ver":"1.2.3",` +
		`"jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r","key":"` + stripeLike + `","n":1700000000123,"id":"1234567890123456"}`
	s := kavach.NewScrubber()
	out := s.String(in)
	for _, leak := range []string{"Lovelace", "7946", "555-0132", "4111 1111", "203.0.113.42", "dBjftJeZ", strings.Repeat("aB3dE", 5)} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked: %s", leak, out)
		}
	}
	for _, keep := range []string{`"lo":"127.0.0.1"`, `"ver":"1.2.3"`, `"n":1700000000123`, `"id":"1234567890123456"`, "sk_live_"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q should survive: %s", keep, out)
		}
	}
	for _, kind := range []string{"email", "phone", "card", "ip", "token"} {
		if s.Counts[kind] == 0 {
			t.Errorf("no %s redaction counted: %v", kind, s.Counts)
		}
	}
	if len(out) == 0 || out[0] != '{' || out[len(out)-1] != '}' {
		t.Errorf("shape not preserved: %s", out)
	}
}

func TestScrubIsStableAndShapePreserving(t *testing.T) {
	s := kavach.NewScrubber()
	a := s.String("ada@example.com and ada@example.com and bob@example.com")
	parts := strings.Split(a, " and ")
	if parts[0] != parts[1] || parts[0] == parts[2] {
		t.Errorf("same value must map to the same placeholder, different to different: %v", parts)
	}
	if !strings.HasSuffix(parts[0], "@example.com") || strings.Contains(parts[0], "ada") {
		t.Errorf("an email must stay an email on the same domain, minus the name: %q", parts[0])
	}
	if again := s.String(a); again != a {
		t.Errorf("scrubbing placeholders must be a no-op: %q -> %q", a, again)
	}
	if other := kavach.NewScrubber().String("ada@example.com"); other == parts[0] {
		t.Error("placeholders must differ between scrubbers (per-file salt)")
	}
	card := s.String("4111-1111-1111-1111")
	if len(card) != 19 || card[0] != '4' || card == "4111-1111-1111-1111" {
		t.Errorf("card: %q", card)
	}
}

func TestScrubLearnsLocalPartsAndSkipsTimestamps(t *testing.T) {
	recs := []journal.Record{
		{Type: journal.TypeInput, Seq: 0, Source: "t", Data: []byte(`{"email":"ann@example.com"}`)},
		{Type: journal.TypeOutput, Seq: 1, Sink: "profiles", Data: []byte(`{"user":"ann","domain":"example.com","at":1700000000004000000,"annual":1}`)},
	}
	_, out := kavach.NewScrubber().Journal(journal.Meta{Service: "x", Start: "genesis"}, recs)
	in, o := string(out[0].Data), string(out[1].Data)
	local := strings.TrimSuffix(strings.TrimPrefix(in, `{"email":"`), `@example.com"}`)
	if strings.Contains(in+o, "ann@") || strings.Contains(o, `"ann"`) {
		t.Errorf("name leaked: %s %s", in, o)
	}
	if !strings.Contains(o, `"user":"`+local+`"`) {
		t.Errorf("the emitted local part must match the scrubbed input (%s): %s", local, o)
	}
	if !strings.Contains(o, "1700000000004000000") || !strings.Contains(o, `"annual":1`) {
		t.Errorf("timestamps and longer words must survive: %s", o)
	}
}

func TestRecorderScrubsByDefault(t *testing.T) {
	t.Setenv("KAVACH_TEST_DB_PASSWORD", "hunter2")
	t.Setenv("KAVACH_TEST_REGION", "eu-1")
	t.Setenv("KAVACH_TEST_CONTACT", "ops@corp.example")
	r := kavach.NewRecorder(newWallet(), testOptions(t))
	var pe *kavach.PanicError
	if err := feed(t, r, "ada@corp.example:10", "bob:null"); !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	raw, err := readAll(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "ada@corp.example", "ops@corp.example"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("%q is in the fixture", leak)
		}
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	m := j.Header.Meta
	if m.Scrub == nil || m.Scrub.Redactions["email"] == 0 || m.Scrub.Redactions["env_secret"] == 0 {
		t.Errorf("scrub summary missing: %+v", m.Scrub)
	}
	if m.Env == nil || !m.Env.Scrubbed || m.Env.Vars["KAVACH_TEST_DB_PASSWORD"] != kavach.RedactedValue || m.Env.Vars["KAVACH_TEST_REGION"] != "eu-1" {
		t.Errorf("env not scrubbed as expected: %+v", m.Env)
	}
}

func TestRecorderNoScrub(t *testing.T) {
	opts := testOptions(t)
	opts.NoScrub = true
	r := kavach.NewRecorder(newWallet(), opts)
	var pe *kavach.PanicError
	if err := feed(t, r, "ada@corp.example:10", "bob:null"); !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	raw, _ := readAll(pe.Fixture)
	if !bytes.Contains(raw, []byte("ada@corp.example")) {
		t.Error("NoScrub must keep the original data")
	}
	j, _ := journal.ReadFile(pe.Fixture)
	if j.Header.Meta.Scrub != nil {
		t.Error("an unscrubbed fixture must not claim a scrub")
	}
}

func TestScrubKeepsReplayVerdict(t *testing.T) {
	r := kavach.NewRecorder(newWallet(), testOptions(t))
	var pe *kavach.PanicError
	if err := feed(t, r, "ada@corp.example:10", "bob@corp.example:5", "ada@corp.example:7", "bob@corp.example:null"); !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	res, err := kavach.Replay(j, newWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusStillFailing {
		t.Errorf("scrubbed fixture no longer reproduces: %s", res)
	}
}

func readAll(path string) ([]byte, error) { return os.ReadFile(path) }
