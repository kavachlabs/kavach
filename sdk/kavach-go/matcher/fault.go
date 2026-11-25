package matcher

import (
	"errors"
	"fmt"
	"strings"
)

// Well-known driver-level fault codes and canonical error strings (SPEC §3.4).
const (
	CodeRedisLoading = "REDIS_LOADING"
	CodePG40001      = "PG_40001"
	CodeETIMEDOUT    = "ETIMEDOUT"

	FaultRedisLoading = "LOADING Redis is loading the dataset in memory"
	FaultPG40001      = "ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)"
	FaultETIMEDOUT    = "ETIMEDOUT: connection timed out"
)

// ExpandFault expands a short fault code (REDIS_LOADING, PG_40001, ETIMEDOUT)
// to its realistic driver error string, or returns any custom error string as-is.
func ExpandFault(codeOrMessage string) string {
	switch strings.ToUpper(strings.TrimSpace(codeOrMessage)) {
	case CodeRedisLoading:
		return FaultRedisLoading
	case CodePG40001:
		return FaultPG40001
	case CodeETIMEDOUT:
		return FaultETIMEDOUT
	default:
		return codeOrMessage
	}
}

// DetectFaultCode classifies a driver error string into a well-known fault code
// if it matches one, or returns "" for custom errors.
func DetectFaultCode(msg string) string {
	upper := strings.ToUpper(msg)
	switch {
	case strings.Contains(upper, "REDIS_LOADING") || strings.Contains(upper, "LOADING REDIS IS LOADING"):
		return CodeRedisLoading
	case strings.Contains(upper, "PG_40001") || strings.Contains(upper, "SQLSTATE 40001") || strings.Contains(upper, "40001"):
		return CodePG40001
	case strings.Contains(upper, "ETIMEDOUT") || strings.Contains(upper, "TIMED OUT") || strings.Contains(upper, "CONTEXT DEADLINE EXCEEDED"):
		return CodeETIMEDOUT
	default:
		return ""
	}
}

// FaultError is returned when a driver-level fault is injected during replay.
type FaultError struct {
	Code    string
	Message string
	Driver  string
	Gateway string
}

// Error implements error. It returns the realistic driver error message so
// service retry logic classifies it identically to production.
func (e *FaultError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return ExpandFault(e.Code)
}

// IsFault reports whether err matches the given fault code or substring.
func IsFault(err error, code string) bool {
	if err == nil {
		return false
	}
	var fe *FaultError
	if errors.As(err, &fe) && strings.EqualFold(fe.Code, code) {
		return true
	}
	return strings.EqualFold(DetectFaultCode(err.Error()), code) ||
		strings.Contains(strings.ToUpper(err.Error()), strings.ToUpper(code))
}

// FaultRule configures a driver-level fault injection rule inside a StepTable.
type FaultRule struct {
	// Driver filters by driver name (e.g. "redis", "pg", "sql", "http"). Empty matches any.
	Driver string
	// Gateway filters by exact gateway name (e.g. "redis:zadd", "pg:select"). Empty matches any.
	Gateway string
	// OpContains filters by case-insensitive substring on Op or NormalizedOp. Empty matches any.
	OpContains string
	// Nth selects the 1-based matching call to fault (0 or 1 means the 1st matching call).
	Nth int
	// ErrString is a fault code ("REDIS_LOADING", "PG_40001", "ETIMEDOUT") or any custom error string.
	ErrString string
	// Once fires the rule once and disables it afterward (useful for retry testing).
	Once bool

	matchesSeen int
	fired       bool
}

// Match evaluates this rule against call, advancing its internal match counter
// and marking it fired when triggered. Callers sharing a *FaultRule across
// goroutines must synchronize access.
func (r *FaultRule) Match(call GatewayCall) bool {
	if r == nil || r.ErrString == "" {
		return false
	}
	return r.matches(call.Canonicalize())
}

// Reset clears the rule's match counter and fired state so it can be reused in
// a fresh replay session.
func (r *FaultRule) Reset() {
	if r == nil {
		return
	}
	r.matchesSeen = 0
	r.fired = false
}

func (r *FaultRule) matches(call GatewayCall) bool {
	if r.Once && r.fired {
		return false
	}
	if r.Driver != "" && !strings.EqualFold(r.Driver, call.Driver) {
		return false
	}
	if r.Gateway != "" && !strings.EqualFold(r.Gateway, call.Gateway) {
		return false
	}
	if r.OpContains != "" {
		needle := strings.ToLower(r.OpContains)
		if !strings.Contains(strings.ToLower(call.Op), needle) &&
			!strings.Contains(strings.ToLower(call.NormalizedOp), needle) {
			return false
		}
	}
	r.matchesSeen++
	target := r.Nth
	if target <= 0 {
		target = 1
	}
	if r.matchesSeen != target {
		return false
	}
	r.fired = true
	return true
}

// UnmatchedGatewayError is returned when a replay issues a Gateway call that
// does not match any recorded call under StrictSeq or KeyedTemplate.
type UnmatchedGatewayError struct {
	Driver       string
	Gateway      string
	Op           string
	NormalizedOp string
	KeyArgs      []string
	StepSeq      uint64
	CallIndex    int
	Available    []string
}

// Error implements error with actionable diagnostics for the developer.
func (e *UnmatchedGatewayError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kavach: unmatched gateway call")
	if e.StepSeq > 0 {
		fmt.Fprintf(&b, " in step %d", e.StepSeq)
	}
	fmt.Fprintf(&b, " (#%d): driver=%q gateway=%q op=%q", e.CallIndex, e.Driver, e.Gateway, e.NormalizedOp)
	if len(e.KeyArgs) > 0 {
		fmt.Fprintf(&b, " keys=%v", e.KeyArgs)
	}
	if len(e.Available) > 0 {
		fmt.Fprintf(&b, "; remaining recorded gateways: [%s]", strings.Join(e.Available, ", "))
	} else {
		b.WriteString("; no unconsumed recorded gateways remain in this step")
	}
	return b.String()
}
