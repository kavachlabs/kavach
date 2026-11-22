package replay_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func TestIsMetadataKey(t *testing.T) {
	wantTrue := []string{
		"email", "EMAIL", "customer_email", "customerEmail", "billing_email",
		"customer_name", "full_name", "first_name", "last_name",
		"phone", "phone_number", "ssn", "iban", "card_pan", "pan",
		"ip", "ip_address", "client_ip",
		"trace_id", "span_id", "request_id", "correlation_id",
		"X-Request-Id", "X-Correlation-ID", "user-agent", "User-Agent",
		"api_key", "api_token", "APIKey", "jwt", "authorization",
		"password", "secret", "client_retry", "request_seq", "cost_center",
		"payload.customer_email", "splits[0].trace_id", "headers.X-Request-Id",
	}
	for _, k := range wantTrue {
		if !replay.IsMetadataKey(k) {
			t.Errorf("IsMetadataKey(%q) = false, want true", k)
		}
	}

	wantFalse := []string{
		"", "[0]", "id", "amount", "currency", "status", "argv",
		"account_id", "user_id", "order_id", "account_name", "event_seq",
		"email_verified", "password_hash", "idempotency_key", "span", "tip", "company",
		"payload.amount", "splits[0].account_id",
	}
	for _, k := range wantFalse {
		if replay.IsMetadataKey(k) {
			t.Errorf("IsMetadataKey(%q) = true, want false", k)
		}
	}
}

func makeJSONIncidentJournal(inputs []string, clocksPerStep []int64, stepReads [][]journal.Record) *journal.Journal {
	var recs []journal.Record
	var seq uint64
	recs = append(recs, journal.Record{Type: journal.TypeEnvironment, Seq: seq})
	seq++
	for i, in := range inputs {
		recs = append(recs, journal.Record{
			Type:     journal.TypeInput,
			Seq:      seq,
			Source:   "api",
			Position: "1",
			Data:     []byte(in),
		})
		seq++
		if i < len(clocksPerStep) && clocksPerStep[i] > 0 {
			recs = append(recs, journal.Record{
				Type:      journal.TypeClock,
				Seq:       seq,
				UnixNanos: clocksPerStep[i],
			})
			seq++
		}
		if i < len(stepReads) {
			for _, r := range stepReads[i] {
				r.Seq = seq
				seq++
				recs = append(recs, r)
			}
		}
		if i == len(inputs)-1 {
			recs = append(recs, journal.Record{
				Type:    journal.TypeMarker,
				Seq:     seq,
				Kind:    journal.MarkerError,
				Message: "boom",
			})
			seq++
		}
	}
	return &journal.Journal{
		Header: journal.Header{
			Major: journal.Major,
			Minor: journal.Minor,
			Meta: journal.Meta{
				Service: "test",
				Start:   journal.StartGenesis,
			},
		},
		Records: recs,
	}
}

func TestMetadataKeyDemotionAndAntiPadding(t *testing.T) {
	t.Run("mixed semantic and metadata keys", func(t *testing.T) {
		j := makeJSONIncidentJournal([]string{
			`{"amount":50,"customer_email":"old@example.com","trace_id":"tr-1"}`,
			`{"amount":100,"customer_email":"fail@example.com","trace_id":"tr-2"}`,
		}, nil, nil)
		vs, err := replay.Variants(j)
		if err != nil {
			t.Fatal(err)
		}
		var fieldCount, pairCount int
		for _, v := range vs {
			info := v.Header.Meta.Variant
			if info.Kind == replay.KindField {
				fieldCount++
			}
			if info.Kind == replay.KindPair {
				pairCount++
			}
			if info.Kind == replay.KindField || info.Kind == replay.KindPair {
				if strings.Contains(info.Mutation, "customer_email") || strings.Contains(info.Mutation, "trace_id") {
					t.Errorf("metadata key mutated when semantic fields exist: %s", info.Mutation)
				}
			}
		}
		if fieldCount == 0 {
			t.Fatal("expected semantic field variants")
		}
		if pairCount != 0 {
			t.Errorf("expected 0 pair variants when only 1 semantic key exists, got %d", pairCount)
		}
	})

	t.Run("metadata-only fixture falls back to seen-value swaps without nearby", func(t *testing.T) {
		j := makeJSONIncidentJournal([]string{
			`{"email":"a@example.com","trace_id":"t1"}`,
			`{"email":"b@example.com","trace_id":"t2"}`,
		}, nil, nil)
		vs, err := replay.Variants(j)
		if err != nil {
			t.Fatal(err)
		}
		var fieldMuts []string
		for _, v := range vs {
			info := v.Header.Meta.Variant
			if info.Kind == replay.KindPair {
				t.Errorf("unexpected pair mutation on metadata-only fixture: %s", info.Mutation)
			}
			if info.Kind == replay.KindField {
				fieldMuts = append(fieldMuts, info.Mutation)
				if strings.Contains(info.Mutation, "-1") || strings.Contains(info.Mutation, "removed") {
					t.Errorf("metadata-only fixture emitted synthetic nearby/removal: %s", info.Mutation)
				}
			}
		}
		if len(fieldMuts) != 2 {
			t.Fatalf("got %d metadata seen-swap variants (%v), want 2", len(fieldMuts), fieldMuts)
		}
	})
}

func TestNestedJSONAndArrayOfObjectsMutations(t *testing.T) {
	j := makeJSONIncidentJournal([]string{
		`{"type":"transfer","payload":{"amount":100,"currency":"USD","memo":"ok"},"splits":[{"account":"a1","amount":50}],"meta":{"trace_id":"t0"}}`,
		`{"type":"transfer","payload":{"amount":200,"currency":"EUR"},"splits":[{"account":"a2","amount":75}],"meta":{"trace_id":"t1"}}`,
	}, nil, nil)
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind == replay.KindField || info.Kind == replay.KindPair {
			got[info.Mutation] = string(recordAt(t, v, info.Incident).Data)
			if strings.Contains(info.Mutation, "trace_id") {
				t.Errorf("nested metadata key should not be mutated: %s", info.Mutation)
			}
		}
	}

	wantSub := map[string]string{
		`"payload.amount" 200 → 100`:       `{"type":"transfer","payload":{"amount":100,"currency":"EUR"},"splits":[{"account":"a2","amount":75}],"meta":{"trace_id":"t1"}}`,
		`"payload.amount" 200 → 201`:       `{"type":"transfer","payload":{"amount":201,"currency":"EUR"},"splits":[{"account":"a2","amount":75}],"meta":{"trace_id":"t1"}}`,
		`"payload.currency" "EUR" → "USD"`: `{"type":"transfer","payload":{"amount":200,"currency":"USD"},"splits":[{"account":"a2","amount":75}],"meta":{"trace_id":"t1"}}`,
		`"payload.memo": "ok" added`:       `{"type":"transfer","payload":{"amount":200,"currency":"EUR","memo":"ok"},"splits":[{"account":"a2","amount":75}],"meta":{"trace_id":"t1"}}`,
		`"splits[0].amount" 75 → 50`:       `{"type":"transfer","payload":{"amount":200,"currency":"EUR"},"splits":[{"account":"a2","amount":50}],"meta":{"trace_id":"t1"}}`,
		`"splits[0].account" "a2" → "a1"`:  `{"type":"transfer","payload":{"amount":200,"currency":"EUR"},"splits":[{"account":"a1","amount":75}],"meta":{"trace_id":"t1"}}`,
	}
	for suffix, wantData := range wantSub {
		found := false
		for mut, data := range got {
			if strings.HasSuffix(mut, ": "+suffix) {
				found = true
				if data != wantData {
					t.Errorf("variant %q:\n got  %s\n want %s", mut, data, wantData)
				}
			}
		}
		if !found {
			t.Errorf("missing expected nested mutation %q in %v", suffix, keys(got))
		}
	}

	// Verify depth limit (up to depth 4) doesn't recurse beyond depth 4 AND skips
	// depth-capped containers rather than swapping them as opaque leaves.
	deepJ := makeJSONIncidentJournal([]string{
		`{"a":{"b":{"c":{"d":{"e":1}}}},"bulk":{"transactions":[{"destinations":[{"identifier":"@A"}]}]}}`,
		`{"a":{"b":{"c":{"d":{"e":2}}}},"bulk":{"transactions":[{"destinations":[{"identifier":"@B"}]}]}}`,
	}, nil, nil)
	deepVs, err := replay.Variants(deepJ)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range deepVs {
		mut := v.Header.Meta.Variant.Mutation
		if strings.Contains(mut, "a.b.c.d") || strings.Contains(mut, "bulk.transactions[0].destinations") {
			t.Errorf("depth-capped container should not be mutated or swapped wholesale: %s", mut)
		}
	}
}

func TestNestedJSONRegressionVariants(t *testing.T) {
	h := kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				Amount int `json:"amount"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(in.Data, &ev); err != nil {
			return err
		}
		if ev.Payload.Amount < 0 {
			return errors.New("negative")
		}
		env.Emit("ok", []byte("1"))
		return nil
	})
	j := recordJournal(t, h,
		`{"type":"transfer","payload":{"amount":50}}`,
		`{"type":"transfer","payload":{"amount":-5}}`,
	)
	rvs, err := replay.RegressionVariants(j, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rvs) == 0 {
		t.Fatal("expected regression variants for nested JSON")
	}
	foundNested := false
	for _, rv := range rvs {
		mut := rv.Header.Meta.Variant.Mutation
		if strings.Contains(mut, `"payload.amount"`) {
			foundNested = true
		}
	}
	if !foundNested {
		t.Errorf("expected nested payload.amount regression variant, got none")
	}
}

func TestFormatAwareNearbyAndNumericBoundaries(t *testing.T) {
	j := makeJSONIncidentJournal([]string{
		`{"ts":"2026-01-15T10:00:00Z","ts_nano":"2026-01-15T10:00:00.001Z","uuid":"550e8400-e29b-41d4-a716-446655440000","curr":"USD","n":3,"z":0,"f":2.5}`,
	}, nil, nil)
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	var muts []string
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind == replay.KindField {
			muts = append(muts, info.Mutation)
		}
	}
	joined := strings.Join(muts, "\n")

	// RFC3339 timestamps must produce valid shifted timestamps, never "-1".
	for _, want := range []string{
		`"ts" "2026-01-15T10:00:00Z" → "2026-01-15T10:00:00.001Z"`,
		`"ts" "2026-01-15T10:00:00Z" → "2026-01-15T09:59:59.999Z"`,
		`"ts" "2026-01-15T10:00:00Z" → "2026-01-15T10:00:01Z"`,
		`"ts" "2026-01-15T10:00:00Z" → "2026-01-15T09:59:59Z"`,
		`"ts_nano" "2026-01-15T10:00:00.001Z" → "2026-01-15T10:00:00.002Z"`,
		`"ts_nano" "2026-01-15T10:00:00.001Z" → "2026-01-15T10:00:00Z"`,
		`"ts_nano" "2026-01-15T10:00:00.001Z" → "2026-01-15T10:00:01.001Z"`,
		`"ts_nano" "2026-01-15T10:00:00.001Z" → "2026-01-15T09:59:59.001Z"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing timestamp shift %s in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, `Z-1"`) || strings.Contains(joined, `446655440000-1`) || strings.Contains(joined, `USD-1`) {
		t.Errorf("corrupted timestamp/UUID/currency with -1:\n%s", joined)
	}

	// Verify UUID and ISO currency only swap seen values (none here, so only removed).
	for _, m := range muts {
		if (strings.Contains(m, `"uuid"`) || strings.Contains(m, `"curr"`)) && !strings.HasSuffix(m, "removed") {
			t.Errorf("unexpected synthetic nearby for uuid/currency: %s", m)
		}
	}

	// Integer and float boundaries.
	for _, want := range []string{
		`"n" 3 → 4`, `"n" 3 → -3`, `"n" 3 → 2`, `"n" 3 → 0`, `"n" 3 → 1`, `"n" 3 → 6`,
		`"z" 0 → 1`, `"z" 0 → -1`,
		`"f" 2.5 → 5`, `"f" 2.5 → 2.51`, `"f" 2.5 → 2.49`, `"f" 2.5 → 0`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing numeric boundary %s in:\n%s", want, joined)
		}
	}
}

func TestRelativeClockSkew(t *testing.T) {
	t0 := int64(1_700_000_000_000_000_000)
	t1 := t0 + int64(5*time.Second)
	j := makeJSONIncidentJournal([]string{
		`{"op":"a"}`,
		`{"op":"b"}`,
	}, []int64{t0, t1}, nil)

	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	skewClocks := map[string]int64{}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind != replay.KindClock || !strings.HasPrefix(info.Mutation, "failing step clock skewed by ") {
			continue
		}
		var clocks []int64
		for _, r := range v.Records {
			if r.Type == journal.TypeClock {
				clocks = append(clocks, r.UnixNanos)
			}
		}
		if len(clocks) != 2 {
			t.Fatalf("variant %s: got %d clock records, want 2", info.Mutation, len(clocks))
		}
		if clocks[0] != t0 {
			t.Errorf("variant %s mutated prior step clock: got %d, want %d", info.Mutation, clocks[0], t0)
		}
		if clocks[1] < clocks[0] {
			t.Errorf("variant %s violated clock monotonicity: %d < %d", info.Mutation, clocks[1], clocks[0])
		}
		skewClocks[info.Mutation] = clocks[1]
	}
	for label, d := range map[string]time.Duration{
		"failing step clock skewed by +1ms": time.Millisecond,
		"failing step clock skewed by +1s":  time.Second,
		"failing step clock skewed by +1m":  time.Minute,
		"failing step clock skewed by +1h":  time.Hour,
	} {
		got, ok := skewClocks[label]
		if !ok {
			t.Errorf("missing relative clock skew %q", label)
		} else if got != t1+int64(d) {
			t.Errorf("%s: got %d, want %d", label, got, t1+int64(d))
		}
	}
}

func TestCausalDropsAndEnhancedDuplicates(t *testing.T) {
	j := makeJSONIncidentJournal([]string{
		`{"op":"create","account_id":"acct_setup"}`,
		`{"op":"ping","account_id":"acct_unrelated"}`,
		`{"op":"debit","account_id":"acct_setup","amount":10}`,
	}, []int64{1000, 2000, 3000}, nil)

	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}

	var dropMuts, dupMuts []string
	var failingDup *journal.Journal
	var delayedDup *journal.Journal
	for _, v := range vs {
		info := v.Header.Meta.Variant
		switch info.Kind {
		case replay.KindDrop:
			dropMuts = append(dropMuts, info.Mutation)
		case replay.KindDuplicate:
			dupMuts = append(dupMuts, info.Mutation)
			if strings.HasPrefix(info.Mutation, "failing input") {
				failingDup = v
			}
			if strings.Contains(info.Mutation, "delayed before") {
				delayedDup = v
			}
		}
	}

	// Causal drops: seq 3 (acct_unrelated) must precede seq 1 (acct_setup), and "all 2" at end.
	if len(dropMuts) != 3 {
		t.Fatalf("got %d drops (%v), want 3", len(dropMuts), dropMuts)
	}
	if dropMuts[0] != "input seq 3 dropped" || dropMuts[1] != "input seq 1 dropped" || dropMuts[2] != "all 2 inputs before the failing one dropped" {
		t.Errorf("unexpected causal drop order: %v", dropMuts)
	}

	// Failing-step redelivery: two copies of the failing input, second marked as Incident.
	if failingDup == nil {
		t.Fatalf("missing failing-step duplicate in %v", dupMuts)
	}
	var inputSeqs []uint64
	for _, r := range failingDup.Records {
		if r.Type == journal.TypeInput {
			inputSeqs = append(inputSeqs, r.Seq)
		}
	}
	if len(inputSeqs) != 4 {
		t.Fatalf("failingDup has %d inputs, want 4", len(inputSeqs))
	}
	if failingDup.Header.Meta.Variant.Incident != inputSeqs[3] {
		t.Errorf("failingDup Incident = %d, want second copy seq %d (first was %d)", failingDup.Header.Meta.Variant.Incident, inputSeqs[3], inputSeqs[2])
	}

	// Delayed redelivery: seq 1 repeated right before failing step, clocks monotonic.
	if delayedDup == nil {
		t.Fatalf("missing delayed duplicate in %v", dupMuts)
	}
	var delayedInputs []string
	var lastClock int64
	for _, r := range delayedDup.Records {
		if r.Type == journal.TypeInput {
			delayedInputs = append(delayedInputs, string(r.Data))
		}
		if r.Type == journal.TypeClock {
			if r.UnixNanos < lastClock {
				t.Errorf("delayedDup clock went backwards: %d < %d", r.UnixNanos, lastClock)
			}
			lastClock = r.UnixNanos
		}
	}
	if len(delayedInputs) != 4 || !strings.Contains(delayedInputs[2], "acct_setup") || !strings.Contains(delayedInputs[3], "debit") {
		t.Errorf("delayedDup input order unexpected: %v", delayedInputs)
	}
}

func TestRegressionSignPreservingNumericMutations(t *testing.T) {
	h := kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		var ev struct {
			Kind   string `json:"kind"`
			Labels []struct {
				Weight int     `json:"weight"`
				Ratio  float64 `json:"ratio"`
			} `json:"labels"`
		}
		if err := json.Unmarshal(in.Data, &ev); err != nil {
			return err
		}
		if len(ev.Labels) > 0 && (ev.Labels[0].Weight < 0 || ev.Labels[0].Ratio <= 0) {
			return errors.New("invalid non-positive weight/ratio")
		}
		if ev.Kind == "hard" {
			return errors.New("hard failure")
		}
		env.Emit("ok", []byte("1"))
		return nil
	})
	j := recordJournal(t, h,
		`{"kind":"soft","labels":[{"weight":100,"ratio":1.5}]}`,
		`{"kind":"hard","labels":[{"weight":100,"ratio":1.5}]}`,
	)
	rvs, err := replay.RegressionVariants(j, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rvs) == 0 {
		t.Fatal("expected regression variants")
	}
	for _, rv := range rvs {
		mut := rv.Header.Meta.Variant.Mutation
		if strings.Contains(mut, "→ -100") || strings.Contains(mut, "→ 0") || strings.Contains(mut, "→ -1.5") {
			t.Errorf("regression variant flipped positive number sign or zeroed it: %s", mut)
		}
	}
}

func TestSymbolicHandlePreservation(t *testing.T) {
	// Step 0 uses @VendorClean and @PartnerContractor;
	// Step 1 uses @PartnerContractor twice (both inside depth-5 destinations and depth-4 source)
	// and @CrossOnly once (cross-step handle).
	j := makeJSONIncidentJournal([]string{
		`{"op":"create","bulk":{"transactions":[{"source":"@Main","destinations":[{"identifier":"@VendorClean"}]},{"source":"@VendorClean","destinations":[{"identifier":"@CrossOnly"}]}]}}`,
		`{"op":"bulk","amount":50,"bulk":{"transactions":[{"source":"@Main","destinations":[{"identifier":"@PartnerContractor"}]},{"source":"@PartnerContractor","destinations":[{"identifier":"@Out"}]}]},"ref":"@CrossOnly"}`,
	}, nil, nil)

	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind != replay.KindField && info.Kind != replay.KindPair {
			continue
		}
		if strings.Contains(info.Mutation, "@PartnerContractor-1") || strings.Contains(info.Mutation, "@CrossOnly-1") {
			t.Errorf("appended -1 to symbolic handle: %s", info.Mutation)
		}
		if strings.Contains(info.Mutation, `"bulk.transactions[1].source"`) && strings.Contains(info.Mutation, "@PartnerContractor") {
			t.Errorf("broke intra-step shared handle link in single occurrence: %s", info.Mutation)
		}
	}
}

func TestPairMutationsUseTopTwoReplacements(t *testing.T) {
	j := makeJSONIncidentJournal([]string{
		`{"precision":100,"tag":"t7"}`,
	}, nil, nil)
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind == replay.KindPair {
			pairs = append(pairs, info.Mutation)
		}
	}
	joined := strings.Join(pairs, "\n")
	if !strings.Contains(joined, `"precision" 100 → 101, "tag" "t7" → "t7-1"`) {
		t.Errorf("missing vi[0] pair mutation in:\n%s", joined)
	}
	if !strings.Contains(joined, `"precision" 100 → -100, "tag" "t7" → "t7-1"`) {
		t.Errorf("missing vi[1] pair mutation in:\n%s", joined)
	}
	if strings.Contains(joined, `"precision" 100 → 200`) {
		t.Errorf("pairMutations selected tail n*2=200 instead of vi[1]:\n%s", joined)
	}
}
