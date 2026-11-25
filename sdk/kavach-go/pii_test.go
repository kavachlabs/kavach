package kavach

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPIIScrubbingStandardDeterministicAndReplaySafe(t *testing.T) {
	key := []byte("test-hmac-secret-key")
	payload := []byte(`{
		"transaction_id": "tx-vertex-003",
		"status": "OK",
		"currency": "USD",
		"customer_email": "alice@acme.io",
		"backup_email": "alice@acme.io",
		"phone": "+14155550199",
		"ssn": "123-45-6789",
		"card_number": "4532 0151 1283 0366",
		"authorization": "Bearer supersecretvalue12345",
		"api_key": "sk_live_99999999999999999",
		"notes": "Contact alice@acme.io or +14155550199 about card 4532-0151-1283-0366 and token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.sig1234"
	}`)

	out1 := ScrubBytes(ScrubStandard, key, payload)
	out2 := ScrubBytes(ScrubStandard, key, payload)
	if !bytes.Equal(out1, out2) {
		t.Fatalf("scrubbing is not deterministic:\n  1: %s\n  2: %s", out1, out2)
	}

	s := string(out1)
	for _, rawSecret := range []string{
		"alice@acme.io",
		"+14155550199",
		"123-45-6789",
		"4532 0151 1283 0366",
		"4532-0151-1283-0366",
		"supersecretvalue12345",
		"sk_live_99999999999999999",
		"eyJhbGciOiJIUzI1NiJ9",
	} {
		if strings.Contains(s, rawSecret) {
			t.Fatalf("PII leaked in ScrubStandard output: %q\nfull: %s", rawSecret, s)
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(out1, &doc); err != nil {
		t.Fatalf("scrubbed output is not valid JSON: %v\n%s", err, out1)
	}
	// Structural/allowlisted join keys preserved untouched.
	if doc["transaction_id"] != "tx-vertex-003" || doc["status"] != "OK" || doc["currency"] != "USD" {
		t.Fatalf("allowlisted keys were altered: %+v", doc)
	}
	// Deterministic leaf tokenization: same email gets identical redacted token everywhere.
	em1, _ := doc["customer_email"].(string)
	em2, _ := doc["backup_email"].(string)
	if em1 == "" || em1 != em2 || !strings.HasSuffix(em1, "@redacted.invalid") {
		t.Fatalf("email tokens not deterministic/formatted: %q vs %q", em1, em2)
	}
	notes, _ := doc["notes"].(string)
	if !strings.Contains(notes, em1) {
		t.Fatalf("inline email token %q does not match leaf token in notes: %q", em1, notes)
	}
	ssn, _ := doc["ssn"].(string)
	if !strings.HasPrefix(ssn, "900-00-") {
		t.Fatalf("unexpected SSN token format: %q", ssn)
	}
	phone, _ := doc["phone"].(string)
	if !strings.HasPrefix(phone, "+155501") {
		t.Fatalf("unexpected phone token format: %q", phone)
	}
}

func TestPIIScrubStrictAndHeadersAndOff(t *testing.T) {
	key := []byte("strict-key")
	raw := []byte(`{"id":"sub-1","status":"active","amount":42,"custom_note":"internal customer memo"}`)

	if off := ScrubBytes(ScrubOff, key, raw); !bytes.Equal(off, raw) {
		t.Fatalf("ScrubOff modified bytes")
	}

	strict := ScrubBytes(ScrubStrict, key, raw)
	var doc map[string]any
	if err := json.Unmarshal(strict, &doc); err != nil {
		t.Fatalf("strict JSON invalid: %v", err)
	}
	if doc["id"] != "sub-1" || doc["status"] != "active" || doc["amount"].(float64) != 42 {
		t.Fatalf("ScrubStrict altered allowlisted/numeric fields: %+v", doc)
	}
	if note, _ := doc["custom_note"].(string); !strings.HasPrefix(note, "tok_") {
		t.Fatalf("ScrubStrict did not tokenize custom_note: %q", note)
	}

	hdrs := ScrubHeaders(ScrubStandard, key, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer secret_jwt_token_999",
		"X-Api-Key":     "my-secret-api-key",
	})
	if hdrs["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type altered: %q", hdrs["Content-Type"])
	}
	if strings.Contains(hdrs["Authorization"], "secret_jwt_token_999") || strings.Contains(hdrs["X-Api-Key"], "my-secret-api-key") {
		t.Fatalf("sensitive headers leaked: %+v", hdrs)
	}
}

func TestPIIScrubBytesPreservesJSONKeyOrder(t *testing.T) {
	key := []byte("lago-order-key")
	// Deliberately non-alphabetical struct field order (matching Go struct json.Marshal).
	type Props struct {
		ZCustomerEmail string `json:"z_customer_email"`
		AUnits         int    `json:"a_units"`
		MContactPhone  string `json:"m_contact_phone"`
	}
	type EnrichedEvent struct {
		OrganizationID  string   `json:"organization_id"`
		TransactionID   string   `json:"transaction_id"`
		AggregationType string   `json:"aggregation_type"`
		Properties      Props    `json:"properties"`
		Emails          []string `json:"emails"`
	}

	orig := EnrichedEvent{
		OrganizationID:  "org-vertex",
		TransactionID:   "tx-vertex-003",
		AggregationType: "sum",
		Properties: Props{
			ZCustomerEmail: "billing@vertex-ai.example",
			AUnits:         25500,
			MContactPhone:  "+14155550199",
		},
		Emails: []string{"billing@vertex-ai.example"},
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal orig: %v", err)
	}

	scrubbed := ScrubBytes(ScrubStandard, key, raw)
	s := string(scrubbed)
	if strings.Contains(s, "billing@vertex-ai.example") || strings.Contains(s, "+14155550199") {
		t.Fatalf("PII leaked: %s", s)
	}

	// Verify exact key order is preserved (not alphabetized!).
	idxOrg := strings.Index(s, `"organization_id"`)
	idxTx := strings.Index(s, `"transaction_id"`)
	idxAgg := strings.Index(s, `"aggregation_type"`)
	idxProps := strings.Index(s, `"properties"`)
	idxZEmail := strings.Index(s, `"z_customer_email"`)
	idxAUnits := strings.Index(s, `"a_units"`)
	idxMPhone := strings.Index(s, `"m_contact_phone"`)
	if !(idxOrg >= 0 && idxOrg < idxTx && idxTx < idxAgg && idxAgg < idxProps && idxProps < idxZEmail && idxZEmail < idxAUnits && idxAUnits < idxMPhone) {
		t.Fatalf("JSON key order was not preserved:\n  orig:     %s\n  scrubbed: %s", raw, scrubbed)
	}

	// Simulate Replay: unmarshal scrubbed JSON back into Go struct and re-marshal.
	// Bytes must match 100% identically (zero divergence!).
	var replayed EnrichedEvent
	if err := json.Unmarshal(scrubbed, &replayed); err != nil {
		t.Fatalf("Unmarshal scrubbed: %v", err)
	}
	remarshaled, err := json.Marshal(replayed)
	if err != nil {
		t.Fatalf("Marshal replayed: %v", err)
	}
	if !bytes.Equal(scrubbed, remarshaled) {
		t.Fatalf("replayed struct marshal diverged from scrubbed JSON:\n  scrubbed:    %s\n  remarshaled: %s", scrubbed, remarshaled)
	}
}
