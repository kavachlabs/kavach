package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// matchGateway implements two-tier Gateway matching within a single step:
//   - Tier 1 (StrictSeq): matches the next unconsumed gateway record by
//     (gateway, exact request or op_fingerprint).
//   - Tier 2 (KeyedTemplate): searches all unconsumed gateway records in the
//     step by exact match first, then by normalized (gateway, op + key args).
//     If matched out of sequential order, records non-fatal ReorderedGateways
//     telemetry on the step.
//
// Returns (rec, matched, nextUnreadIdx).
func (e *replayEnv) matchGateway(gateway string, request []byte) (journal.Record, bool, int) {
	nextIdx := e.nextUnusedGateway()
	if nextIdx < 0 {
		return journal.Record{}, false, -1
	}

	// Tier 1: StrictSeq against the next unconsumed gateway record.
	if gatewayStrictMatch(e.gateways[nextIdx], gateway, request) {
		e.gwUsed[nextIdx] = true
		return e.gateways[nextIdx], true, nextIdx
	}

	// Tier 2a: Exact (gateway, request / op_fingerprint) match at another
	// unconsumed slot (e.g., two reads reordered by a patch).
	for i, r := range e.gateways {
		if e.gwUsed[i] || i == nextIdx {
			continue
		}
		if gatewayStrictMatch(r, gateway, request) {
			e.gwUsed[i] = true
			e.reorderedGateways++
			return r, true, nextIdx
		}
	}

	// Tier 2b: Normalized KeyedTemplate match — check nextIdx first (in-order
	// canonicalization), then any unconsumed slot (reordered).
	reqTpl := gatewayKeyedTemplate(gateway, request)
	if sameGatewayName(e.gateways[nextIdx].Gateway, gateway) &&
		gatewayKeyedTemplate(e.gateways[nextIdx].Gateway, e.gateways[nextIdx].Request) == reqTpl {
		e.gwUsed[nextIdx] = true
		return e.gateways[nextIdx], true, nextIdx
	}
	for i, r := range e.gateways {
		if e.gwUsed[i] || i == nextIdx {
			continue
		}
		if sameGatewayName(r.Gateway, gateway) &&
			gatewayKeyedTemplate(r.Gateway, r.Request) == reqTpl {
			e.gwUsed[i] = true
			e.reorderedGateways++
			return r, true, nextIdx
		}
	}

	return journal.Record{}, false, nextIdx
}

func (e *replayEnv) nextUnusedGateway() int {
	for i := range e.gateways {
		if !e.gwUsed[i] {
			return i
		}
	}
	return -1
}

func (e *replayEnv) takeUnreadGateway(match func(journal.Record) bool) (journal.Record, bool) {
	for i, r := range e.gateways {
		if !e.gwUsed[i] && match(r) {
			e.gwUsed[i] = true
			return r, true
		}
	}
	return journal.Record{}, false
}

// lenientFallbackGateway resolves a gateway call on a lenient step after
// StrictSeq and KeyedTemplate both missed (e.g. a variant mutated key_args or
// a SQL parameter shifted). It prefers an unread record with the same
// normalized operation so queries with different schemas never cross-match.
func (e *replayEnv) lenientFallbackGateway(gateway string, request []byte) (journal.Record, bool) {
	reqOp, hasOp := gatewayNormalizedOp(gateway, request)
	if hasOp {
		// 1. Prefer an unread gateway matching (sameGatewayName, same normalized_op).
		if rec, ok := e.takeUnreadGateway(func(r journal.Record) bool {
			if !sameGatewayName(r.Gateway, gateway) {
				return false
			}
			recOp, rok := gatewayNormalizedOp(r.Gateway, r.Request)
			return rok && recOp == reqOp
		}); ok {
			return rec, true
		}
		// 2. For SQL gateways, NEVER cross-match against a different SQL
		// statement/column schema. Reuse the last matching same-normalized_op
		// record in the step (preferring a non-error response) if one exists;
		// otherwise report no recorded response.
		if isSQLGateway(gateway) {
			var fallback journal.Record
			found := false
			for _, r := range e.gateways {
				if !sameGatewayName(r.Gateway, gateway) {
					continue
				}
				recOp, rok := gatewayNormalizedOp(r.Gateway, r.Request)
				if !rok || recOp != reqOp {
					continue
				}
				if r.Error == "" || !found {
					fallback = r
					found = true
				}
			}
			if found {
				return fallback, true
			}
			return journal.Record{}, false
		}
	}
	// Fallback for opaque/unstructured gateways (e.g. SPEC §6 lenient replay).
	return e.takeUnreadGateway(func(r journal.Record) bool { return sameGatewayName(r.Gateway, gateway) })
}

func isSQLGateway(gateway string) bool {
	switch driverOf(gateway) {
	case "pg", "pgx", "postgres", "postgresql", "sql", "mysql", "sqlite":
		return true
	}
	return false
}

// gatewayNormalizedOp extracts the operation template (independent of key_args
// / bind parameters) from a structured gateway request. Returns ("", false)
// for opaque payloads so callers fall back to raw gateway-name matching.
func gatewayNormalizedOp(gateway string, req []byte) (string, bool) {
	trimmed := bytes.TrimSpace(req)
	if len(trimmed) == 0 {
		return "", false
	}
	drv := driverOf(gateway)
	if trimmed[0] == '{' {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if dec.Decode(&m) == nil {
			if isSQLGateway(gateway) {
				for _, k := range []string{"normalized_op", "sql", "query", "statement", "op"} {
					if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
						return normalizeSQLText(s), true
					}
				}
				return "", false
			}
			if drv == "redis" {
				for _, k := range []string{"normalized_op", "cmd", "command", "op"} {
					if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
						return strings.ToUpper(strings.TrimSpace(s)), true
					}
				}
				return "", false
			}
			if s, ok := m["normalized_op"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), true
			}
			if drv == "http" || drv == "https" {
				method, _ := m["method"].(string)
				urlStr, _ := m["url"].(string)
				if urlStr == "" {
					urlStr, _ = m["path"].(string)
				}
				if method != "" && urlStr != "" {
					if qi := strings.IndexByte(urlStr, '?'); qi >= 0 {
						urlStr = urlStr[:qi]
					}
					return strings.ToUpper(strings.TrimSpace(method)) + " " + urlStr, true
				}
			}
			return "", false
		}
	}
	if isSQLGateway(gateway) {
		return normalizeSQLText(string(trimmed)), true
	}
	return "", false
}

func gatewayStrictMatch(rec journal.Record, gateway string, request []byte) bool {
	if rec.Gateway != gateway {
		return false
	}
	if bytes.Equal(rec.Request, request) {
		return true
	}
	// If both requests carry an explicit op_fingerprint, match on it.
	if fpA, okA := extractOpFingerprint(rec.Request); okA {
		if fpB, okB := extractOpFingerprint(request); okB && fpA == fpB {
			return true
		}
	}
	return false
}

func extractOpFingerprint(req []byte) (string, bool) {
	if len(req) < 2 || req[0] != '{' {
		return "", false
	}
	var m map[string]any
	if json.Unmarshal(req, &m) != nil {
		return "", false
	}
	for _, k := range []string{"op_fingerprint", "fingerprint"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

func driverOf(gateway string) string {
	drv := gateway
	if idx := strings.IndexByte(drv, ':'); idx >= 0 {
		drv = drv[:idx]
	}
	return strings.ToLower(strings.TrimSpace(drv))
}

func sameGatewayName(a, b string) bool {
	if a == b {
		return true
	}
	da, db := driverOf(a), driverOf(b)
	if da != db {
		return false
	}
	switch da {
	case "redis", "pg", "pgx", "postgres", "postgresql", "sql", "http", "https":
		return strings.EqualFold(a, b)
	}
	return false
}

var redisKeyOnlyCommands = map[string]bool{
	"ZADD": true, "ZREM": true, "ZRANGEBYSCORE": true, "ZREMRANGEBYSCORE": true,
	"SET": true, "SETEX": true, "PSETEX": true, "SETNX": true,
	"EXPIRE": true, "PEXPIRE": true, "EXPIREAT": true, "PEXPIREAT": true,
	"HSET": true, "INCR": true, "INCRBY": true, "DECR": true, "DECRBY": true,
	"LPUSH": true, "RPUSH": true, "SADD": true, "SREM": true, "PUBLISH": true,
}

var sqlKeywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "AND": true, "OR": true, "NOT": true,
	"IN": true, "IS": true, "NULL": true, "TRUE": true, "FALSE": true,
	"AS": true, "ON": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true, "OUTER": true,
	"ORDER": true, "BY": true, "GROUP": true, "HAVING": true, "LIMIT": true, "OFFSET": true,
	"DISTINCT": true, "EXISTS": true, "CASE": true, "WHEN": true, "THEN": true, "ELSE": true, "END": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true, "DELETE": true, "RETURNING": true,
	"ASC": true, "DESC": true, "UNION": true, "ALL": true, "FOR": true, "BETWEEN": true, "LIKE": true, "ILIKE": true,
}

var volatileHTTPHeaders = map[string]bool{
	"x-request-id":     true,
	"x-correlation-id": true,
	"traceparent":      true,
	"tracestate":       true,
	"x-b3-traceid":     true,
	"x-b3-spanid":      true,
	"x-b3-sampled":     true,
	"date":             true,
	"user-agent":       true,
}

// gatewayKeyedTemplate computes a canonical (normalized_op, key_args) signature
// for Tier 2 fallback matching. Opaque non-JSON payloads for unknown gateways
// are returned verbatim so unrelated requests never collide.
func gatewayKeyedTemplate(gateway string, req []byte) string {
	drv := driverOf(gateway)
	trimmed := bytes.TrimSpace(req)
	if len(trimmed) == 0 {
		return ""
	}
	switch drv {
	case "redis":
		return normalizeRedisTemplate(gateway, trimmed)
	case "pg", "pgx", "postgres", "postgresql", "sql", "mysql", "sqlite":
		return normalizeSQLTemplate(trimmed)
	case "http", "https":
		return normalizeHTTPTemplate(trimmed)
	}
	// Generic JSON canonicalization (key order & whitespace independent) while
	// keeping plain opaque bytes exact.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if canon, ok := canonicalizeJSONValue(trimmed); ok {
			return canon
		}
	}
	return string(req)
}

func normalizeRedisTemplate(gateway string, req []byte) string {
	cmdFromGW := ""
	if idx := strings.IndexByte(gateway, ':'); idx >= 0 {
		cmdFromGW = strings.ToUpper(strings.TrimSpace(gateway[idx+1:]))
	}
	if req[0] == '{' {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(req))
		dec.UseNumber()
		if dec.Decode(&m) == nil {
			cmd := cmdFromGW
			for _, k := range []string{"normalized_op", "cmd", "command", "op"} {
				if s, ok := m[k].(string); ok && s != "" {
					cmd = strings.ToUpper(strings.TrimSpace(s))
					break
				}
			}
			if key, ok := m["key"]; ok && key != nil {
				return "redis:" + cmd + ":" + mustCanonical(key)
			}
			if keys, ok := m["keys"]; ok && keys != nil {
				return "redis:" + cmd + ":" + mustCanonical(keys)
			}
			if args, ok := m["args"].([]any); ok {
				return redisCmdArgsKey(cmd, args)
			}
			if args, ok := m["key_args"].([]any); ok {
				return redisCmdArgsKey(cmd, args)
			}
			return "redis:" + cmd + ":" + mustCanonical(m)
		}
	}
	if req[0] == '[' {
		var arr []any
		dec := json.NewDecoder(bytes.NewReader(req))
		dec.UseNumber()
		if dec.Decode(&arr) == nil && len(arr) > 0 {
			cmd := cmdFromGW
			rest := arr
			if s, ok := arr[0].(string); ok {
				cmd = strings.ToUpper(strings.TrimSpace(s))
				rest = arr[1:]
			}
			return redisCmdArgsKey(cmd, rest)
		}
	}
	// Plain text Redis command, e.g. "ZADD subscription_refreshed_v2 1700000 sub|1".
	fields := strings.Fields(string(req))
	if len(fields) == 0 {
		return "redis:" + cmdFromGW
	}
	cmd := strings.ToUpper(fields[0])
	rest := make([]any, 0, len(fields)-1)
	for _, f := range fields[1:] {
		rest = append(rest, f)
	}
	return redisCmdArgsKey(cmd, rest)
}

func redisCmdArgsKey(cmd string, args []any) string {
	if len(args) == 0 {
		return "redis:" + cmd
	}
	if redisKeyOnlyCommands[cmd] {
		return "redis:" + cmd + ":" + mustCanonical(args[0])
	}
	return "redis:" + cmd + ":" + mustCanonical(args)
}

func normalizeSQLTemplate(req []byte) string {
	if req[0] == '{' {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(req))
		dec.UseNumber()
		if dec.Decode(&m) == nil {
			clean := make(map[string]any, len(m))
			for k, v := range m {
				switch k {
				case "duration_ms", "latency_ms", "trace_id", "span_id", "request_id", "op_fingerprint", "fingerprint", "driver":
					continue
				case "normalized_op", "sql", "query", "statement", "op":
					if s, ok := v.(string); ok {
						clean["sql"] = normalizeSQLText(s)
						continue
					}
				case "params", "args", "key_args":
					clean["args"] = v
					continue
				}
				clean[k] = v
			}
			return "sql:" + mustCanonical(clean)
		}
	}
	return "sql:" + normalizeSQLText(string(req))
}

func normalizeSQLText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	fields := strings.Fields(s)
	for i, f := range fields {
		upper := strings.ToUpper(f)
		if sqlKeywords[upper] {
			fields[i] = upper
		}
	}
	return strings.Join(fields, " ")
}

func normalizeHTTPTemplate(req []byte) string {
	if req[0] == '{' {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(req))
		dec.UseNumber()
		if dec.Decode(&m) == nil {
			clean := make(map[string]any, len(m))
			for k, v := range m {
				switch k {
				case "method":
					if s, ok := v.(string); ok {
						clean[k] = strings.ToUpper(strings.TrimSpace(s))
						continue
					}
				case "headers":
					if hm, ok := v.(map[string]any); ok {
						ch := map[string]any{}
						for hk, hv := range hm {
							lk := strings.ToLower(hk)
							if volatileHTTPHeaders[lk] {
								continue
							}
							ch[lk] = hv
						}
						clean[k] = ch
						continue
					}
				case "duration_ms", "latency_ms", "trace_id", "span_id", "request_id":
					continue
				}
				clean[k] = v
			}
			return "http:" + mustCanonical(clean)
		}
	}
	return string(req)
}

func canonicalizeJSONValue(b []byte) (string, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	return mustCanonical(v), true
}

func mustCanonical(v any) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, v)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, _ := json.Marshal(k)
			buf.Write(b)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, x[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, e)
		}
		buf.WriteByte(']')
	case json.Number:
		buf.WriteString(x.String())
	default:
		b, err := json.Marshal(x)
		if err != nil {
			fmt.Fprintf(buf, "%q", fmt.Sprint(x))
			return
		}
		buf.Write(b)
	}
}

// sameOutputsPerSink reports whether want and got have the exact same outputs
// within every sink in the same per-sink order, allowing independent sinks
// emitted concurrently (e.g. via errgroup) to finish in any relative order
// without causing false divergence.
func sameOutputsPerSink(want, got []kavach.Output) bool {
	if len(want) != len(got) {
		return false
	}
	if len(want) <= 1 {
		return false
	}
	wantBySink := make(map[string][][]byte)
	gotBySink := make(map[string][][]byte)
	for _, o := range want {
		wantBySink[o.Sink] = append(wantBySink[o.Sink], o.Data)
	}
	for _, o := range got {
		gotBySink[o.Sink] = append(gotBySink[o.Sink], o.Data)
	}
	if len(wantBySink) != len(gotBySink) {
		return false
	}
	for sink, ws := range wantBySink {
		gs := gotBySink[sink]
		if len(ws) != len(gs) {
			return false
		}
		for i := range ws {
			if !bytes.Equal(ws[i], gs[i]) {
				return false
			}
		}
	}
	return true
}
