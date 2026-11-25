// Package matcher implements per-step separate Gateway and Output tables,
// two-tier Gateway matching (StrictSeq + KeyedTemplate fallback), clear
// UnmatchedGateway diagnostics, and driver-level fault injection.
package matcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"
)

// NormalizeOp canonicalizes a driver operation into a stable template suitable
// for StrictSeq fingerprinting and KeyedTemplate fallback matching.
func NormalizeOp(driver, op string) string {
	op = strings.TrimSpace(op)
	if op == "" {
		return ""
	}
	d := strings.ToLower(strings.TrimSpace(driver))
	switch d {
	case "pg", "postgres", "postgresql", "pgx", "sql":
		return NormalizeSQL(op)
	case "redis":
		// Keep command name uppercased; drop volatile args/scores/members.
		fields := strings.Fields(op)
		if len(fields) == 0 {
			return ""
		}
		return strings.ToUpper(fields[0])
	case "http", "https":
		fields := strings.Fields(op)
		if len(fields) >= 2 {
			return strings.ToUpper(fields[0]) + " " + stripQueryString(fields[1])
		}
		return op
	default:
		return collapseSpaces(op)
	}
}

// NormalizeSQL normalizes SQL queries by collapsing whitespace, stripping SQL
// comments, replacing string literals, numeric literals, and positional
// placeholders ($1, $2, ...) with "?".
func NormalizeSQL(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	n := len(sql)
	inSpace := false
	writeSpace := func() {
		if b.Len() > 0 && !inSpace {
			b.WriteByte(' ')
			inSpace = true
		}
	}
	for i := 0; i < n; {
		c := sql[i]
		// Line comment: -- ...
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			for i < n && sql[i] != '\n' {
				i++
			}
			writeSpace()
			continue
		}
		// Block comment: /* ... */
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			writeSpace()
			continue
		}
		// Whitespace
		if unicode.IsSpace(rune(c)) {
			writeSpace()
			i++
			continue
		}
		// Single-quoted string literal -> ?
		if c == '\'' {
			i++
			for i < n {
				if sql[i] == '\'' {
					if i+1 < n && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte('?')
			inSpace = false
			continue
		}
		// Postgres positional placeholder $1, $2, ... -> ?
		if c == '$' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9' {
			i++
			for i < n && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			b.WriteByte('?')
			inSpace = false
			continue
		}
		// Standalone numeric literal (not preceded by identifier char) -> ?
		if c >= '0' && c <= '9' && (i == 0 || !isIdentChar(sql[i-1])) {
			j := i
			for j < n && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			if j < n && sql[j] == '.' && j+1 < n && sql[j+1] >= '0' && sql[j+1] <= '9' {
				j++
				for j < n && sql[j] >= '0' && sql[j] <= '9' {
					j++
				}
			}
			if j == n || !isIdentChar(sql[j]) {
				b.WriteByte('?')
				inSpace = false
				i = j
				continue
			}
		}
		b.WriteByte(c)
		inSpace = false
		i++
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimRight(out, ";")
	return strings.TrimSpace(out)
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func stripQueryString(u string) string {
	if idx := strings.IndexByte(u, '?'); idx >= 0 {
		return u[:idx]
	}
	return u
}

// SQLKind returns a short operation verb for a SQL query (select, insert, update, delete, exec).
func SQLKind(sql string) string {
	fields := strings.Fields(strings.TrimSpace(sql))
	if len(fields) == 0 {
		return "query"
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "WITH", "SHOW", "EXPLAIN", "VALUES":
		return "select"
	case "INSERT":
		return "insert"
	case "UPDATE":
		return "update"
	case "DELETE":
		return "delete"
	default:
		return "exec"
	}
}

// ComputeFingerprint computes a deterministic fingerprint for (driver, normalizedOp, keyArgs).
func ComputeFingerprint(driver, normalizedOp string, keyArgs []string) string {
	h := sha256.New()
	writePart := func(s string) {
		var lenBuf [4]byte
		n := len(s)
		lenBuf[0] = byte(n >> 24)
		lenBuf[1] = byte(n >> 16)
		lenBuf[2] = byte(n >> 8)
		lenBuf[3] = byte(n)
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	writePart(strings.ToLower(strings.TrimSpace(driver)))
	writePart(normalizedOp)
	for _, k := range keyArgs {
		writePart(k)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// canonicalReqEnvelope is the wire JSON envelope used when an adapter does not
// provide raw request bytes. It omits volatile fields (such as wall-clock scores)
// so StrictSeq and KeyedTemplate matching stay deterministic across replays.
type canonicalReqEnvelope struct {
	Driver        string   `json:"driver"`
	Op            string   `json:"op,omitempty"`
	NormalizedOp  string   `json:"normalized_op,omitempty"`
	KeyArgs       []string `json:"key_args,omitempty"`
	OpFingerprint string   `json:"op_fingerprint,omitempty"`
}

// ParseGatewayRequest populates a GatewayCall from a wire (gateway, request) pair
// recorded in a Kavach journal or sent over the host protocol.
func ParseGatewayRequest(gateway string, request []byte) GatewayCall {
	gc := GatewayCall{
		Gateway: gateway,
		Request: cloneBytes(request),
	}
	if len(request) > 0 && request[0] == '{' {
		var env struct {
			Driver        string   `json:"driver"`
			Op            string   `json:"op"`
			SQL           string   `json:"sql"`
			Cmd           string   `json:"cmd"`
			Method        string   `json:"method"`
			URL           string   `json:"url"`
			NormalizedOp  string   `json:"normalized_op"`
			KeyArgs       []string `json:"key_args"`
			Key           string   `json:"key"`
			OpFingerprint string   `json:"op_fingerprint"`
		}
		if json.Unmarshal(request, &env) == nil {
			if env.Driver != "" {
				gc.Driver = env.Driver
			}
			switch {
			case env.Op != "":
				gc.Op = env.Op
			case env.SQL != "":
				gc.Op = env.SQL
			case env.Cmd != "":
				gc.Op = env.Cmd
			case env.Method != "" && env.URL != "":
				gc.Op = env.Method + " " + env.URL
			}
			if env.NormalizedOp != "" {
				gc.NormalizedOp = env.NormalizedOp
			}
			if len(env.KeyArgs) > 0 {
				gc.KeyArgs = env.KeyArgs
			} else if env.Key != "" {
				gc.KeyArgs = []string{env.Key}
			}
			if env.OpFingerprint != "" {
				gc.OpFingerprint = env.OpFingerprint
			}
		}
	}
	return gc.Canonicalize()
}
