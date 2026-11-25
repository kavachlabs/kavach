package kavach

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type piiClass uint8

const (
	piiNone piiClass = iota
	piiEmail
	piiSSN
	piiPhone
	piiCard
	piiGeneric
)

var allowlistedEnumKeys = map[string]bool{
	"type":                     true,
	"kind":                     true,
	"status":                   true,
	"currency":                 true,
	"op":                       true,
	"action":                   true,
	"code":                     true,
	"event":                    true,
	"method":                   true,
	"path":                     true,
	"topic":                    true,
	"driver":                   true,
	"sink":                     true,
	"id":                       true,
	"transaction_id":           true,
	"subscription_id":          true,
	"external_subscription_id": true,
	"organization_id":          true,
	"plan_id":                  true,
}

var genericSensitiveKeyTokens = []string{
	"_tax_id_", "_iban_", "_routing_", "_account_number_",
	"_name_", "_names_", "_first_name_", "_last_name_", "_full_name_",
	"_address_", "_addresses_", "_street_", "_dob_", "_birth_",
	"_password_", "_passwords_", "_passwd_",
	"_secret_", "_secrets_", "_client_secret_", "_private_key_", "_access_key_",
	"_token_", "_tokens_", "_api_key_", "_apikey_",
	"_authorization_", "_auth_", "_bearer_",
	"_cookie_", "_cookies_", "_ip_", "_ip_address_", "_dsn_",
}

// ScrubBytes deterministically scrubs PII from data according to mode and HMAC key.
// JSON objects/arrays are scrubbed at leaf granularity while preserving exact
// original object key order, numbers, and formatting so Go struct marshaling
// replays byte-for-byte identically. Non-JSON text is scrubbed inline.
func ScrubBytes(mode ScrubMode, key []byte, data []byte) []byte {
	if mode == ScrubOff || len(data) == 0 {
		return data
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if out, changed, ok := scrubJSONPreservingOrder(data, mode, key); ok {
			if !changed {
				return data
			}
			return out
		}
	}
	if s, changed := scrubInlineText(string(data), key); changed {
		return []byte(s)
	}
	return data
}

// ScrubString deterministically scrubs a single string value associated with keyName.
func ScrubString(mode ScrubMode, key []byte, keyName, val string) string {
	if mode == ScrubOff || val == "" {
		return val
	}
	if cls := detectLeafClass(keyName, val, mode); cls != piiNone {
		return tokenizeLeaf(val, cls, key)
	}
	if out, changed := scrubInlineText(val, key); changed {
		return out
	}
	return val
}

// ScrubHeaders returns a scrubbed copy of HTTP/Kafka headers, deterministically
// tokenizing sensitive headers (Authorization, Cookie, X-Api-Key, etc.) and any
// inline PII values.
func ScrubHeaders(mode ScrubMode, key []byte, headers map[string]string) map[string]string {
	if mode == ScrubOff || len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = ScrubString(mode, key, k, v)
	}
	return out
}

const maxJSONScrubDepth = 64

type jsonScrubber struct {
	src     []byte
	pos     int
	out     bytes.Buffer
	copied  int
	changed bool
	mode    ScrubMode
	key     []byte
}

// scrubJSONPreservingOrder parses RFC 8259 JSON in-place and rewrites only the
// string leaves whose scrubbed value differs, preserving exact key order,
// whitespace, and non-string tokens.
func scrubJSONPreservingOrder(src []byte, mode ScrubMode, key []byte) ([]byte, bool, bool) {
	s := &jsonScrubber{src: src, mode: mode, key: key}
	s.skipWS()
	if !s.scanValue("", 0) {
		return nil, false, false
	}
	s.skipWS()
	if s.pos != len(s.src) {
		return nil, false, false
	}
	if !s.changed {
		return src, false, true
	}
	if s.copied < len(s.src) {
		s.out.Write(s.src[s.copied:])
	}
	return s.out.Bytes(), true, true
}

func (s *jsonScrubber) skipWS() {
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScrubber) scanValue(keyName string, depth int) bool {
	if depth > maxJSONScrubDepth || s.pos >= len(s.src) {
		return false
	}
	switch s.src[s.pos] {
	case '{':
		return s.scanObject(depth + 1)
	case '[':
		return s.scanArray(keyName, depth+1)
	case '"':
		return s.scanStringLeaf(keyName)
	case 't':
		if s.pos+4 <= len(s.src) && string(s.src[s.pos:s.pos+4]) == "true" {
			s.pos += 4
			return true
		}
		return false
	case 'f':
		if s.pos+5 <= len(s.src) && string(s.src[s.pos:s.pos+5]) == "false" {
			s.pos += 5
			return true
		}
		return false
	case 'n':
		if s.pos+4 <= len(s.src) && string(s.src[s.pos:s.pos+4]) == "null" {
			s.pos += 4
			return true
		}
		return false
	default:
		return s.scanNumber()
	}
}

func (s *jsonScrubber) scanObject(depth int) bool {
	s.pos++ // '{'
	s.skipWS()
	if s.pos < len(s.src) && s.src[s.pos] == '}' {
		s.pos++
		return true
	}
	for {
		s.skipWS()
		if s.pos >= len(s.src) || s.src[s.pos] != '"' {
			return false
		}
		keyStart := s.pos
		if !s.advanceString() {
			return false
		}
		var fieldKey string
		if err := json.Unmarshal(s.src[keyStart:s.pos], &fieldKey); err != nil {
			return false
		}
		s.skipWS()
		if s.pos >= len(s.src) || s.src[s.pos] != ':' {
			return false
		}
		s.pos++
		s.skipWS()
		if !s.scanValue(fieldKey, depth) {
			return false
		}
		s.skipWS()
		if s.pos >= len(s.src) {
			return false
		}
		if s.src[s.pos] == ',' {
			s.pos++
			continue
		}
		if s.src[s.pos] == '}' {
			s.pos++
			return true
		}
		return false
	}
}

func (s *jsonScrubber) scanArray(keyName string, depth int) bool {
	s.pos++ // '['
	s.skipWS()
	if s.pos < len(s.src) && s.src[s.pos] == ']' {
		s.pos++
		return true
	}
	for {
		s.skipWS()
		if !s.scanValue(keyName, depth) {
			return false
		}
		s.skipWS()
		if s.pos >= len(s.src) {
			return false
		}
		if s.src[s.pos] == ',' {
			s.pos++
			continue
		}
		if s.src[s.pos] == ']' {
			s.pos++
			return true
		}
		return false
	}
}

func (s *jsonScrubber) scanStringLeaf(keyName string) bool {
	start := s.pos
	if !s.advanceString() {
		return false
	}
	end := s.pos
	var rawVal string
	if err := json.Unmarshal(s.src[start:end], &rawVal); err != nil {
		return false
	}
	scrubbed := ScrubString(s.mode, s.key, keyName, rawVal)
	if scrubbed == rawVal {
		return true
	}
	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		return false
	}
	if !s.changed {
		s.out.Grow(len(s.src) + 32)
		s.changed = true
	}
	if start > s.copied {
		s.out.Write(s.src[s.copied:start])
	}
	s.out.Write(encoded)
	s.copied = end
	return true
}

func (s *jsonScrubber) advanceString() bool {
	if s.pos >= len(s.src) || s.src[s.pos] != '"' {
		return false
	}
	s.pos++
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if c == '"' {
			s.pos++
			return true
		}
		if c == '\\' {
			s.pos += 2
			if s.pos > len(s.src) {
				return false
			}
			continue
		}
		if c < 0x20 {
			return false
		}
		s.pos++
	}
	return false
}

func (s *jsonScrubber) scanNumber() bool {
	start := s.pos
	if s.pos < len(s.src) && s.src[s.pos] == '-' {
		s.pos++
	}
	if s.pos >= len(s.src) || !isASCIIDigit(s.src[s.pos]) {
		return false
	}
	if s.src[s.pos] == '0' {
		s.pos++
	} else {
		for s.pos < len(s.src) && isASCIIDigit(s.src[s.pos]) {
			s.pos++
		}
	}
	if s.pos < len(s.src) && s.src[s.pos] == '.' {
		s.pos++
		if s.pos >= len(s.src) || !isASCIIDigit(s.src[s.pos]) {
			return false
		}
		for s.pos < len(s.src) && isASCIIDigit(s.src[s.pos]) {
			s.pos++
		}
	}
	if s.pos < len(s.src) && (s.src[s.pos] == 'e' || s.src[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.src) && (s.src[s.pos] == '+' || s.src[s.pos] == '-') {
			s.pos++
		}
		if s.pos >= len(s.src) || !isASCIIDigit(s.src[s.pos]) {
			return false
		}
		for s.pos < len(s.src) && isASCIIDigit(s.src[s.pos]) {
			s.pos++
		}
	}
	return s.pos > start
}

func normalizeKeyForPII(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 4)
	b.WriteByte('_')
	lastUnderscore := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isAlpha && !isDigit {
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
			continue
		}
		if !lastUnderscore && c >= 'A' && c <= 'Z' && i > 0 {
			prev := key[i-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
				b.WriteByte('_')
			}
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
		lastUnderscore = false
	}
	if !lastUnderscore {
		b.WriteByte('_')
	}
	return b.String()
}

func classifySensitiveKey(key string) piiClass {
	if key == "" {
		return piiNone
	}
	norm := normalizeKeyForPII(key)
	for _, p := range []string{"_email_", "_emails_", "_mail_"} {
		if strings.Contains(norm, p) {
			return piiEmail
		}
	}
	if strings.Contains(norm, "_ssn_") {
		return piiSSN
	}
	for _, p := range []string{"_phone_", "_phones_", "_mobile_"} {
		if strings.Contains(norm, p) {
			return piiPhone
		}
	}
	for _, p := range []string{"_pan_", "_card_", "_cards_", "_cc_num_", "_card_number_", "_credit_card_"} {
		if strings.Contains(norm, p) {
			return piiCard
		}
	}
	for _, p := range genericSensitiveKeyTokens {
		if strings.Contains(norm, p) {
			return piiGeneric
		}
	}
	return piiNone
}

func detectLeafClass(key, val string, mode ScrubMode) piiClass {
	if val == "" {
		return piiNone
	}
	if isAllowlistedKey(key) {
		// Allowlisted keys are never redacted by key name, only if the value itself
		// is an unmistakable PII pattern.
		return detectValueClass(val)
	}
	if vc := detectValueClass(val); vc != piiNone {
		return vc
	}
	if kc := classifySensitiveKey(key); kc != piiNone {
		return kc
	}
	if mode == ScrubStrict {
		return piiGeneric
	}
	return piiNone
}

func isAllowlistedKey(key string) bool {
	if key == "" {
		return false
	}
	return allowlistedEnumKeys[strings.ToLower(strings.TrimSpace(key))]
}

func detectValueClass(val string) piiClass {
	v := strings.TrimSpace(val)
	if v == "" {
		return piiNone
	}
	if isWholeEmail(v) {
		return piiEmail
	}
	if isWholeSSN(v) {
		return piiSSN
	}
	if isWholePhone(v) {
		return piiPhone
	}
	if isLuhnCard(v) {
		return piiCard
	}
	if isWholeJWT(v) || isBearerSecret(v) || isAPIKeyToken(v) {
		return piiGeneric
	}
	return piiNone
}

func tokenizeLeaf(val string, cls piiClass, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(val))
	sum := mac.Sum(nil)
	hex16 := hex.EncodeToString(sum[:8])
	num := binary.BigEndian.Uint64(sum[:8])

	switch cls {
	case piiEmail:
		return "u_" + hex16 + "@redacted.invalid"
	case piiSSN:
		return fmt.Sprintf("900-00-%04d", num%10000)
	case piiPhone:
		return fmt.Sprintf("+155501%05d", num%100000)
	default:
		return "tok_" + hex16
	}
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }
func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isASCIIAlnum(c byte) bool { return isASCIIAlpha(c) || isASCIIDigit(c) }
func isB64URL(c byte) bool     { return isASCIIAlnum(c) || c == '-' || c == '_' }

func isWholeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at+1 >= len(s) || strings.IndexByte(s[at+1:], '@') >= 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	dom := s[at+1:]
	dot := strings.IndexByte(dom, '.')
	if dot <= 0 || dot+1 >= len(dom) || dom[len(dom)-1] == '.' || strings.Contains(dom, "..") {
		return false
	}
	lastDot := strings.LastIndexByte(dom, '.')
	tld := dom[lastDot+1:]
	if len(tld) < 2 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		if !isASCIIAlpha(tld[i]) {
			return false
		}
	}
	return true
}

func isWholeSSN(s string) bool {
	if len(s) != 11 || s[3] != '-' || s[6] != '-' {
		return false
	}
	for _, idx := range []int{0, 1, 2, 4, 5, 7, 8, 9, 10} {
		if !isASCIIDigit(s[idx]) {
			return false
		}
	}
	return true
}

func isWholePhone(s string) bool {
	if len(s) < 8 || len(s) > 20 {
		return false
	}
	// E.164: + followed by 8..15 digits (optional spaces/dashes)
	if s[0] == '+' {
		if len(s) < 9 || s[1] < '1' || s[1] > '9' {
			return false
		}
		digits := 0
		for i := 1; i < len(s); i++ {
			c := s[i]
			if isASCIIDigit(c) {
				digits++
			} else if c != '-' && c != ' ' {
				return false
			}
		}
		return digits >= 8 && digits <= 15
	}
	// US parenthesized format: (NNN) NNN-NNNN
	if len(s) == 14 && s[0] == '(' && s[4] == ')' && s[5] == ' ' && s[9] == '-' {
		for _, idx := range []int{1, 2, 3, 6, 7, 8, 10, 11, 12, 13} {
			if !isASCIIDigit(s[idx]) {
				return false
			}
		}
		return true
	}
	return false
}

func isLuhnCard(s string) bool {
	if len(s) < 13 || len(s) > 24 || !isASCIIDigit(s[0]) || !isASCIIDigit(s[len(s)-1]) {
		return false
	}
	var digits [19]int
	n := 0
	prevSep := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIIDigit(c) {
			if n >= 19 {
				return false
			}
			digits[n] = int(c - '0')
			n++
			prevSep = false
		} else if (c == ' ' || c == '-') && !prevSep {
			prevSep = true
		} else {
			return false
		}
	}
	if n < 13 {
		return false
	}
	sum := 0
	dbl := false
	for i := n - 1; i >= 0; i-- {
		x := digits[i]
		if dbl {
			x *= 2
			if x > 9 {
				x -= 9
			}
		}
		sum += x
		dbl = !dbl
	}
	return sum%10 == 0
}

func isWholeJWT(s string) bool {
	if !strings.HasPrefix(s, "eyJ") || len(s) < 10 {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 || len(parts[0]) < 4 || len(parts[1]) == 0 || len(parts[2]) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isB64URL(c) && c != '.' && c != '=' {
			return false
		}
	}
	return true
}

func isBearerSecret(s string) bool {
	if len(s) > 10 && strings.EqualFold(s[:7], "bearer ") {
		tok := strings.TrimSpace(s[7:])
		return len(tok) >= 8 && !strings.ContainsAny(tok, " \t\n\r")
	}
	return false
}

func isAPIKeyToken(s string) bool {
	if len(s) < 16 || strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	for _, prefix := range []string{"sk_live_", "sk_test_", "pk_live_", "pk_test_", "ghp_", "gho_", "glpat-", "AKIA", "xoxb-", "xoxp-"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// scrubInlineText scans free-form text and deterministically replaces inline
// emails, SSNs, E.164 phones, Luhn cards, JWTs, and Bearer tokens.
func scrubInlineText(s string, key []byte) (string, bool) {
	n := len(s)
	if n < 6 {
		return s, false
	}
	var out strings.Builder
	copied := 0
	changed := false

	for i := 0; i < n; {
		c := s[i]
		matchEnd := 0
		cls := piiNone

		// Bearer token
		if (c == 'B' || c == 'b') && i+8 < n && strings.EqualFold(s[i:i+7], "bearer ") &&
			(i == 0 || !isASCIIAlnum(s[i-1])) {
			j := i + 7
			for j < n && !isSpaceByte(s[j]) && s[j] != '"' && s[j] != '\'' && s[j] != ',' && s[j] != ';' {
				j++
			}
			if j-(i+7) >= 8 {
				if !changed {
					out.Grow(n + 32)
					changed = true
				}
				out.WriteString(s[copied : i+7])
				out.WriteString(tokenizeLeaf(s[i+7:j], piiGeneric, key))
				copied = j
				i = j
				continue
			}
		}

		// JWT
		if c == 'e' && i+8 <= n && s[i:i+3] == "eyJ" && (i == 0 || (!isB64URL(s[i-1]) && s[i-1] != '.')) {
			if end := scanInlineJWT(s, i); end > i {
				matchEnd = end
				cls = piiGeneric
			}
		}

		// E.164 Phone
		if matchEnd == 0 && c == '+' && i+9 <= n && s[i+1] >= '1' && s[i+1] <= '9' &&
			(i == 0 || (!isASCIIAlnum(s[i-1]) && s[i-1] != '_' && s[i-1] != '+' && s[i-1] != '-')) {
			if end := scanInlinePhone(s, i); end > i {
				matchEnd = end
				cls = piiPhone
			}
		}

		// SSN or Luhn Card
		if matchEnd == 0 && isASCIIDigit(c) {
			if end := scanInlineSSN(s, i); end > i {
				matchEnd = end
				cls = piiSSN
			} else if end := scanInlineCard(s, i); end > i {
				matchEnd = end
				cls = piiCard
			}
		}

		// Email
		if matchEnd == 0 && (isASCIIAlnum(c) || c == '_') &&
			(i == 0 || (!isASCIIAlnum(s[i-1]) && s[i-1] != '_' && s[i-1] != '@' && s[i-1] != '.')) {
			if end := scanInlineEmail(s, i); end > i {
				matchEnd = end
				cls = piiEmail
			}
		}

		if matchEnd > i {
			if !changed {
				out.Grow(n + 32)
				changed = true
			}
			if i > copied {
				out.WriteString(s[copied:i])
			}
			out.WriteString(tokenizeLeaf(s[i:matchEnd], cls, key))
			copied = matchEnd
			i = matchEnd
		} else {
			i++
		}
	}

	if !changed {
		return s, false
	}
	if copied < n {
		out.WriteString(s[copied:])
	}
	return out.String(), true
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func scanInlineJWT(s string, i int) int {
	j := i
	dots := 0
	for j < len(s) && (isB64URL(s[j]) || s[j] == '.' || s[j] == '=') {
		if s[j] == '.' {
			dots++
		}
		j++
	}
	for j > i && s[j-1] == '.' {
		j--
		dots--
	}
	if dots == 2 && isWholeJWT(s[i:j]) {
		return j
	}
	return 0
}

func scanInlineSSN(s string, i int) int {
	if i+11 > len(s) {
		return 0
	}
	if i > 0 {
		prev := s[i-1]
		if isASCIIAlnum(prev) || prev == '_' || prev == '-' || prev == '+' {
			return 0
		}
	}
	if i+11 < len(s) {
		next := s[i+11]
		if isASCIIAlnum(next) || next == '_' || next == '-' {
			return 0
		}
	}
	if isWholeSSN(s[i : i+11]) {
		return i + 11
	}
	return 0
}

func scanInlinePhone(s string, i int) int {
	j := i + 1
	digits := 0
	for j < len(s) {
		c := s[j]
		if isASCIIDigit(c) {
			digits++
			j++
			if digits > 15 {
				return 0
			}
		} else if (c == '-' || c == ' ') && j+1 < len(s) && isASCIIDigit(s[j+1]) {
			j++
		} else {
			break
		}
	}
	if digits < 8 || digits > 15 {
		return 0
	}
	if j < len(s) && (isASCIIAlnum(s[j]) || s[j] == '_' || s[j] == '@') {
		return 0
	}
	return j
}

func scanInlineCard(s string, i int) int {
	if i+13 > len(s) {
		return 0
	}
	if i > 0 {
		prev := s[i-1]
		if isASCIIAlnum(prev) || prev == '_' || prev == '-' || prev == '+' {
			return 0
		}
	}
	j := i
	digits := 0
	for j < len(s) {
		c := s[j]
		if isASCIIDigit(c) {
			digits++
			j++
			if digits > 19 {
				return 0
			}
		} else if (c == '-' || c == ' ') && j+1 < len(s) && isASCIIDigit(s[j+1]) {
			j++
		} else {
			break
		}
	}
	if digits < 13 || digits > 19 {
		return 0
	}
	if j < len(s) && (isASCIIAlnum(s[j]) || s[j] == '_' || s[j] == '-' || s[j] == '@') {
		return 0
	}
	if isLuhnCard(s[i:j]) {
		return j
	}
	return 0
}

func scanInlineEmail(s string, i int) int {
	n := len(s)
	p := i
	for p < n && (isASCIIAlnum(s[p]) || s[p] == '.' || s[p] == '_' || s[p] == '%' || s[p] == '+' || s[p] == '-') {
		p++
	}
	if p == i || p-i > 64 || p >= n || s[p] != '@' || s[p-1] == '.' {
		return 0
	}
	q := p + 1
	for q < n && (isASCIIAlnum(s[q]) || s[q] == '-' || s[q] == '.') {
		q++
	}
	for q > p+1 && (s[q-1] == '.' || s[q-1] == '-') {
		q--
	}
	if q < n && (isASCIIAlnum(s[q]) || s[q] == '_' || s[q] == '@') {
		return 0
	}
	if isWholeEmail(s[i:q]) {
		return q
	}
	return 0
}
