// Package kavachpgx provides Kavach v0.2 record/replay adapters for both
// github.com/jackc/pgx/v5 (pgxpool / pgx) and database/sql/driver (including
// gorm.io/driver/postgres via postgres.Config{Conn: sqlDB}), with ZERO real
// PostgreSQL connection in Replay mode.
package kavachpgx

import (
	"bytes"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/matcher"
)

// TypedCell preserves exact database/sql/driver.Value & pgx Go types across JSON
// serialization so scanners like sql.NullTime, custom StringArray, []byte, int64,
// float64, bool, string, and nil round-trip losslessly during Replay.
type TypedCell struct {
	Type string `json:"t"`
	Val  any    `json:"v,omitempty"`
}

// RecordedResultSet is the JSON wire format stored in Gateway.Response for SQL queries.
// Supports both TypedCell Rows (database/sql & high-level pgx) and exact pgproto3
// WireRows/ColumnOIDs/ColumnFormats (for full-fidelity *pgxpool.Pool wire replay).
type RecordedResultSet struct {
	Columns       []string      `json:"columns"`
	ColumnOIDs    []uint32      `json:"column_oids,omitempty"`
	ColumnFormats []int16       `json:"column_formats,omitempty"`
	Rows          [][]TypedCell `json:"rows,omitempty"`
	WireRows      [][]*string   `json:"wire_rows,omitempty"`
	CommandTag    string        `json:"command_tag,omitempty"`
	RowsAffected  int64         `json:"rows_affected,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// RecordedExec is the JSON wire format stored in Output.Data for SQL writes ("pg:exec").
type RecordedExec struct {
	SQL           string   `json:"sql"`
	NormalizedSQL string   `json:"normalized_sql"`
	Args          []string `json:"args,omitempty"`
	CommandTag    string   `json:"command_tag,omitempty"`
	RowsAffected  int64    `json:"rows_affected,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func encodeCell(v any) TypedCell {
	if v == nil {
		return TypedCell{Type: "null"}
	}
	switch x := v.(type) {
	case bool:
		return TypedCell{Type: "bool", Val: x}
	case int:
		return TypedCell{Type: "int64", Val: strconv.FormatInt(int64(x), 10)}
	case int8:
		return TypedCell{Type: "int64", Val: strconv.FormatInt(int64(x), 10)}
	case int16:
		return TypedCell{Type: "int64", Val: strconv.FormatInt(int64(x), 10)}
	case int32:
		return TypedCell{Type: "int64", Val: strconv.FormatInt(int64(x), 10)}
	case int64:
		return TypedCell{Type: "int64", Val: strconv.FormatInt(x, 10)}
	case uint:
		return TypedCell{Type: "int64", Val: strconv.FormatUint(uint64(x), 10)}
	case uint8:
		return TypedCell{Type: "int64", Val: strconv.FormatUint(uint64(x), 10)}
	case uint16:
		return TypedCell{Type: "int64", Val: strconv.FormatUint(uint64(x), 10)}
	case uint32:
		return TypedCell{Type: "int64", Val: strconv.FormatUint(uint64(x), 10)}
	case uint64:
		return TypedCell{Type: "int64", Val: strconv.FormatUint(x, 10)}
	case float32:
		return TypedCell{Type: "float64", Val: float64(x)}
	case float64:
		return TypedCell{Type: "float64", Val: x}
	case string:
		return TypedCell{Type: "string", Val: x}
	case []byte:
		return TypedCell{Type: "bytes", Val: base64.StdEncoding.EncodeToString(x)}
	case time.Time:
		return TypedCell{Type: "time", Val: x.UTC().Format(time.RFC3339Nano)}
	case [16]byte:
		return TypedCell{Type: "bytes", Val: base64.StdEncoding.EncodeToString(x[:])}
	default:
		if valuer, ok := v.(driver.Valuer); ok {
			if dv, err := valuer.Value(); err == nil {
				return encodeCell(dv)
			}
		}
		if b, err := json.Marshal(v); err == nil {
			return TypedCell{Type: "bytes", Val: base64.StdEncoding.EncodeToString(b)}
		}
		return TypedCell{Type: "string", Val: fmt.Sprint(v)}
	}
}

func decodeCell(c TypedCell) (any, error) {
	switch c.Type {
	case "", "null":
		return nil, nil
	case "bool":
		switch v := c.Val.(type) {
		case bool:
			return v, nil
		case string:
			return strconv.ParseBool(v)
		}
		return false, nil
	case "int64":
		switch v := c.Val.(type) {
		case string:
			return strconv.ParseInt(v, 10, 64)
		case json.Number:
			return v.Int64()
		case float64:
			return int64(v), nil
		case int64:
			return v, nil
		}
		return int64(0), nil
	case "float64":
		switch v := c.Val.(type) {
		case json.Number:
			return v.Float64()
		case float64:
			return v, nil
		case string:
			return strconv.ParseFloat(v, 64)
		}
		return float64(0), nil
	case "string":
		if c.Val == nil {
			return "", nil
		}
		return fmt.Sprint(c.Val), nil
	case "bytes":
		if c.Val == nil {
			return []byte(nil), nil
		}
		s, ok := c.Val.(string)
		if !ok {
			return []byte(fmt.Sprint(c.Val)), nil
		}
		return base64.StdEncoding.DecodeString(s)
	case "time":
		s, ok := c.Val.(string)
		if !ok || s == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC(), nil
		}
		return time.Parse(time.RFC3339, s)
	default:
		return c.Val, nil
	}
}

func marshalResultSet(rs *RecordedResultSet) ([]byte, error) {
	return json.Marshal(rs)
}

func unmarshalResultSet(data []byte) (*RecordedResultSet, error) {
	if len(data) == 0 {
		return &RecordedResultSet{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var rs RecordedResultSet
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("kavachpgx: decode result set: %w", err)
	}
	return &rs, nil
}

func decodeRowValues(row []TypedCell) ([]any, error) {
	vals := make([]any, len(row))
	for i, c := range row {
		v, err := decodeCell(c)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func sqlClassify(query string) (gateway string, normSQL string, isWrite bool, hasReturning bool) {
	normSQL = matcher.NormalizeSQL(query)
	kind := matcher.SQLKind(normSQL)
	upper := " " + strings.ToUpper(normSQL) + " "
	hasReturning = strings.Contains(upper, " RETURNING ")
	isWrite = kind == "insert" || kind == "update" || kind == "delete" || kind == "exec"
	if !isWrite {
		gateway = "pg:select"
	} else {
		gateway = "pg:" + kind
	}
	return gateway, normSQL, isWrite, hasReturning
}

func formatArg(v any) string {
	if v == nil {
		return "<nil>"
	}
	switch x := v.(type) {
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if x == nil {
			return "<nil>"
		}
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	case string:
		return x
	case driver.Valuer:
		if dv, err := x.Value(); err == nil {
			return formatArg(dv)
		}
		return fmt.Sprint(x)
	default:
		return fmt.Sprint(x)
	}
}

func formatAnyArgs(args []any) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = formatArg(a)
	}
	return out
}

func formatNamedArgs(args []driver.NamedValue) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = formatArg(a.Value)
	}
	return out
}

func buildQueryGatewayCall(query string, args []string) (kavach.GatewayCall, string, bool, bool) {
	gw, normSQL, isWrite, hasReturning := sqlClassify(query)
	call := kavach.GatewayCall{
		Driver:       "pg",
		Gateway:      gw,
		Op:           query,
		NormalizedOp: normSQL,
		KeyArgs:      args,
	}
	return call, normSQL, isWrite, hasReturning
}

func defaultCommandTag(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) == 0 {
		return "OK"
	}
	verb := strings.ToUpper(fields[0])
	switch verb {
	case "INSERT":
		return "INSERT 0 1"
	case "UPDATE":
		return "UPDATE 1"
	case "DELETE":
		return "DELETE 1"
	case "SELECT":
		return "SELECT 0"
	default:
		return verb
	}
}

// isTxControlSQL reports whether normSQL is a transaction/session-control
// statement (e.g. BEGIN, COMMIT, ROLLBACK, SAVEPOINT, RELEASE SAVEPOINT, SET)
// that should not be emitted as a durable "pg:exec" Output.
func isTxControlSQL(normSQL string) bool {
	fields := strings.Fields(strings.TrimSpace(normSQL))
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE",
		"SET", "PREPARE", "DEALLOCATE", "DISCARD", "RESET", "LISTEN", "UNLISTEN", "CLOSE":
		return true
	default:
		return false
	}
}

var sqlStateRe = regexp.MustCompile(`(?i)SQLSTATE\s+([0-9A-Z]{5})`)

// reconstructPGError turns a recorded or injected fault error string (such as
// PG_40001 / SQLSTATE 40001) into a genuine *pgconn.PgError so application retry
// logic using errors.As(err, &pgErr) behaves identically to live PostgreSQL.
func reconstructPGError(err error) error {
	if err == nil {
		return nil
	}
	var existing *pgconn.PgError
	if errors.As(err, &existing) {
		return err
	}
	msg := err.Error()
	code := ""
	if m := sqlStateRe.FindStringSubmatch(msg); len(m) == 2 {
		code = strings.ToUpper(m[1])
	} else if matcher.IsFault(err, kavach.CodePG40001) {
		code = "40001"
	}
	if code == "" {
		return err
	}
	cleanMsg := msg
	cleanMsg = strings.TrimPrefix(cleanMsg, "ERROR: ")
	if idx := strings.LastIndex(strings.ToUpper(cleanMsg), " (SQLSTATE "); idx > 0 {
		cleanMsg = cleanMsg[:idx]
	}
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     code,
		Message:  cleanMsg,
	}
}

// scanInto assigns a decoded cell value into a pgx Scan destination pointer.
func scanInto(src any, dst any) error {
	if dst == nil {
		return nil
	}
	if sc, ok := dst.(interface{ Scan(any) error }); ok {
		return sc.Scan(src)
	}
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("kavachpgx: scan destination must be non-nil pointer, got %T", dst)
	}
	elem := rv.Elem()
	if src == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}
	if elem.Kind() == reflect.Ptr {
		ptr := reflect.New(elem.Type().Elem())
		if err := scanInto(src, ptr.Interface()); err != nil {
			return err
		}
		elem.Set(ptr)
		return nil
	}

	sv := reflect.ValueOf(src)
	if sv.Type().AssignableTo(elem.Type()) {
		elem.Set(sv)
		return nil
	}
	switch elem.Kind() {
	case reflect.String:
		switch s := src.(type) {
		case []byte:
			elem.SetString(string(s))
			return nil
		default:
			elem.SetString(fmt.Sprint(src))
			return nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch n := src.(type) {
		case int64:
			elem.SetInt(n)
			return nil
		case float64:
			elem.SetInt(int64(n))
			return nil
		case string:
			parsed, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return err
			}
			elem.SetInt(parsed)
			return nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch n := src.(type) {
		case int64:
			elem.SetUint(uint64(n))
			return nil
		case float64:
			elem.SetUint(uint64(n))
			return nil
		}
	case reflect.Float32, reflect.Float64:
		switch n := src.(type) {
		case float64:
			elem.SetFloat(n)
			return nil
		case int64:
			elem.SetFloat(float64(n))
			return nil
		}
	case reflect.Bool:
		switch b := src.(type) {
		case bool:
			elem.SetBool(b)
			return nil
		case int64:
			elem.SetBool(b != 0)
			return nil
		}
	case reflect.Slice:
		if elem.Type().Elem().Kind() == reflect.Uint8 {
			if s, ok := src.(string); ok {
				elem.SetBytes([]byte(s))
				return nil
			}
		}
	}
	if sv.Type().ConvertibleTo(elem.Type()) {
		elem.Set(sv.Convert(elem.Type()))
		return nil
	}
	return fmt.Errorf("kavachpgx: cannot scan %T into %T", src, dst)
}
