package kavachpgx

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kavachlabs/kavach-go"
)

// WireOptions customizes the PostgreSQL wire-protocol (pgproto3) *pgxpool.Pool
// adapter used by WrapPGXConfig, NewPGXPool, and NewPGXPoolWithConfig.
type WireOptions struct {
	// ArgNormalizer optionally normalizes bound SQL parameter strings (e.g.,
	// replacing random UUID batch IDs) before Gateway fingerprinting and write Output emission.
	ArgNormalizer func(sql string, args []string) []string
	// WriteSink optionally overrides the Output sink for SQL writes (default: "pg:exec").
	// Returning "" suppresses Output emission for that statement. Returning distinct
	// sinks (e.g. "pg:lease:worker", "pg:lease:queue") lets concurrent goroutines
	// match cleanly under per-sink output comparison.
	WriteSink func(sql string, args []string) string
}

func mergeWireOptions(opts ...WireOptions) WireOptions {
	var out WireOptions
	for _, o := range opts {
		if o.ArgNormalizer != nil {
			out.ArgNormalizer = o.ArgNormalizer
		}
		if o.WriteSink != nil {
			out.WriteSink = o.WriteSink
		}
	}
	return out
}

func resolveWriteSink(opt WireOptions, sqlStr string, args []string) string {
	if opt.WriteSink != nil {
		return opt.WriteSink(sqlStr, args)
	}
	return "pg:exec"
}

// WrapPGXConfig configures cfg.ConnConfig.DialFunc with Kavach's transparent
// PostgreSQL v3 wire-protocol (pgproto3) recorder/replayer so unmodified
// libraries requiring a concrete *pgxpool.Pool run 100% offline in ModeReplay
// and record full-fidelity typed OID/wire rows in ModeRecord.
func WrapPGXConfig(kv *kavach.Kavach, cfg *pgxpool.Config, opts ...WireOptions) {
	if kv == nil || kv.IsOff() || cfg == nil || cfg.ConnConfig == nil {
		return
	}
	opt := mergeWireOptions(opts...)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	if kv.IsReplay() {
		cfg.ConnConfig.TLSConfig = nil
		cfg.ConnConfig.Fallbacks = nil
		cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
			clientConn, serverConn := net.Pipe()
			go serveReplayWireConn(serverConn, kv, opt)
			return clientConn, nil
		}
		return
	}

	origDial := cfg.ConnConfig.DialFunc
	if origDial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		origDial = dialer.DialContext
	}
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		upstream, err := origDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		clientConn, proxyConn := net.Pipe()
		go proxyRecordWireConn(proxyConn, upstream, kv, opt)
		return clientConn, nil
	}
}

// NewPGXPool parses dsn, wraps it via WrapPGXConfig, and returns a concrete *pgxpool.Pool.
// In ModeReplay it NEVER dials the network (even if dsn is empty or unreachable).
func NewPGXPool(ctx context.Context, kv *kavach.Kavach, dsn string, opts ...WireOptions) (*pgxpool.Pool, error) {
	if kv != nil && kv.IsReplay() && strings.TrimSpace(dsn) == "" {
		dsn = "postgres://kavach@127.0.0.1:5432/kavach?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		if kv != nil && kv.IsReplay() {
			cfg, err = pgxpool.ParseConfig("postgres://kavach@127.0.0.1:5432/kavach?sslmode=disable")
		}
		if err != nil {
			return nil, err
		}
	}
	return NewPGXPoolWithConfig(ctx, kv, cfg, opts...)
}

// NewPGXPoolWithConfig wraps cfg via WrapPGXConfig and returns a concrete *pgxpool.Pool.
// In ModeReplay it runs 100% offline over in-process net.Pipe() pgproto3 connections.
func NewPGXPoolWithConfig(ctx context.Context, kv *kavach.Kavach, cfg *pgxpool.Config, opts ...WireOptions) (*pgxpool.Pool, error) {
	if cfg == nil {
		var err error
		cfg, err = pgxpool.ParseConfig("postgres://kavach@127.0.0.1:5432/kavach?sslmode=disable")
		if err != nil {
			return nil, err
		}
	}
	WrapPGXConfig(kv, cfg, opts...)
	return pgxpool.NewWithConfig(ctx, cfg)
}

// --- Record Mode: Transparent pgproto3 Wire Proxy ---

type wireOpKind uint8

const (
	wireOpParse wireOpKind = iota + 1
	wireOpBind
	wireOpDescribePortal
	wireOpDescribeStmt
	wireOpExecute
	wireOpClose
	wireOpSync
	wireOpSimpleQuery
)

type wireOp struct {
	kind      wireOpKind
	sql       string
	args      []string
	described bool
}

type wirePortal struct {
	sql       string
	args      []string
	described bool
}

func proxyRecordWireConn(clientConn, upstream net.Conn, kv *kavach.Kavach, opt WireOptions) {
	defer clientConn.Close()
	defer upstream.Close()

	// Tee every frontend byte verbatim to upstream while decoding messages.
	feReader := io.TeeReader(clientConn, upstream)
	beParser := pgproto3.NewBackend(feReader, io.Discard)

	// Handle initial SSLRequest / GSSEncRequest / StartupMessage handshake.
	for {
		msg, err := beParser.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			var b [1]byte
			if _, err := io.ReadFull(upstream, b[:]); err != nil {
				return
			}
			if _, err := clientConn.Write(b[:]); err != nil {
				return
			}
			if b[0] == 'S' {
				// Encrypted TLS session established: fall back to raw bidirectional passthrough.
				rawPassthrough(clientConn, upstream)
				return
			}
			continue
		case *pgproto3.CancelRequest:
			return
		case *pgproto3.StartupMessage:
			// Proceed to auth + steady-state proxy below.
		default:
			return
		}
		break
	}

	opCh := make(chan wireOp, 256)
	var wg sync.WaitGroup
	wg.Add(1)

	// Goroutine 2: Upstream -> Client.
	// Uses pgproto3.Frontend to decode upstream responses and pgproto3.Backend to
	// forward them to clientConn ONLY AFTER kv.RecordGateway / kv.RecordOutput
	// completes, eliminating any race with StepContext.End().
	go func() {
		defer wg.Done()
		feParser := pgproto3.NewFrontend(upstream, io.Discard)
		clientOut := pgproto3.NewBackend(nil, clientConn)

		// Drain startup/auth handshake until initial ReadyForQuery.
		for {
			msg, err := feParser.Receive()
			if err != nil {
				return
			}
			clientOut.Send(msg)
			if err := clientOut.Flush(); err != nil {
				return
			}
			if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
				break
			}
		}

		nextMsg := func() (pgproto3.BackendMessage, error) {
			for {
				m, err := feParser.Receive()
				if err != nil {
					return nil, err
				}
				switch m.(type) {
				case *pgproto3.NoticeResponse, *pgproto3.ParameterStatus,
					*pgproto3.NotificationResponse, *pgproto3.BackendKeyData:
					clientOut.Send(m)
					continue
				default:
					return m, nil
				}
			}
		}

		var (
			lastFields    []pgproto3.FieldDescription
			skipUntilSync bool
			pendingErr    string
		)

		for op := range opCh {
			if skipUntilSync {
				switch op.kind {
				case wireOpExecute:
					if op.described && pendingErr != "" {
						recordWireGateway(kv, op.sql, op.args, nil, pendingErr)
						pendingErr = ""
					}
				case wireOpSync:
					// Wait for ReadyForQuery signaling end of failed extended pipeline.
					for {
						m, err := nextMsg()
						if err != nil {
							return
						}
						clientOut.Send(m)
						if _, ok := m.(*pgproto3.ReadyForQuery); ok {
							if err := clientOut.Flush(); err != nil {
								return
							}
							break
						}
					}
					skipUntilSync = false
					pendingErr = ""
					lastFields = nil
				}
				continue
			}

			switch op.kind {
			case wireOpParse, wireOpBind, wireOpClose:
				m, err := nextMsg()
				if err != nil {
					return
				}
				clientOut.Send(m)
				if er, ok := m.(*pgproto3.ErrorResponse); ok {
					pendingErr = formatErrorResponse(er)
					skipUntilSync = true
				}

			case wireOpDescribeStmt:
				m, err := nextMsg()
				if err != nil {
					return
				}
				clientOut.Send(m)
				if er, ok := m.(*pgproto3.ErrorResponse); ok {
					pendingErr = formatErrorResponse(er)
					skipUntilSync = true
					continue
				}
				if _, ok := m.(*pgproto3.ParameterDescription); ok {
					m2, err := nextMsg()
					if err != nil {
						return
					}
					switch x := m2.(type) {
					case *pgproto3.RowDescription:
						fields := cloneFieldDescriptions(x.Fields)
						rs := buildWireResultSet(fields, nil, "DESCRIBE", op.sql)
						recordWireDescribeGateway(kv, op.sql, rs, "")
					case *pgproto3.NoData:
						recordWireDescribeGateway(kv, op.sql, &RecordedResultSet{CommandTag: "DESCRIBE"}, "")
					case *pgproto3.ErrorResponse:
						pendingErr = formatErrorResponse(x)
						skipUntilSync = true
					}
					clientOut.Send(m2)
				}

			case wireOpDescribePortal:
				m, err := nextMsg()
				if err != nil {
					return
				}
				switch x := m.(type) {
				case *pgproto3.RowDescription:
					lastFields = cloneFieldDescriptions(x.Fields)
				case *pgproto3.NoData:
					lastFields = nil
				case *pgproto3.ErrorResponse:
					pendingErr = formatErrorResponse(x)
					skipUntilSync = true
					lastFields = nil
				}
				clientOut.Send(m)

			case wireOpExecute:
				var wireRows [][]*string
				var cmdTag string
				var errStr string
				var termMsg pgproto3.BackendMessage
			execLoop:
				for {
					m, err := nextMsg()
					if err != nil {
						return
					}
					switch x := m.(type) {
					case *pgproto3.DataRow:
						wireRows = append(wireRows, encodeWireRow(x.Values))
						clientOut.Send(m)
					case *pgproto3.CommandComplete:
						cmdTag = string(x.CommandTag)
						termMsg = m
						break execLoop
					case *pgproto3.PortalSuspended:
						termMsg = m
						break execLoop
					case *pgproto3.EmptyQueryResponse:
						termMsg = m
						break execLoop
					case *pgproto3.ErrorResponse:
						errStr = formatErrorResponse(x)
						skipUntilSync = true
						termMsg = m
						break execLoop
					default:
						clientOut.Send(m)
					}
				}
				recordWireOpResult(kv, opt, op.sql, op.args, op.described, lastFields, wireRows, cmdTag, errStr)
				lastFields = nil
				if termMsg != nil {
					clientOut.Send(termMsg)
				}

			case wireOpSync:
				for {
					m, err := nextMsg()
					if err != nil {
						return
					}
					clientOut.Send(m)
					if _, ok := m.(*pgproto3.ReadyForQuery); ok {
						if err := clientOut.Flush(); err != nil {
							return
						}
						break
					}
				}

			case wireOpSimpleQuery:
				var (
					fields   []pgproto3.FieldDescription
					wireRows [][]*string
					cmdTag   string
					errStr   string
					isCopy   bool
					hasRows  bool
					rfqMsg   pgproto3.BackendMessage
				)
			simpleLoop:
				for {
					m, err := nextMsg()
					if err != nil {
						return
					}
					switch x := m.(type) {
					case *pgproto3.CopyInResponse:
						isCopy = true
						clientOut.Send(m)
						if err := clientOut.Flush(); err != nil {
							return
						}
					case *pgproto3.RowDescription:
						fields = cloneFieldDescriptions(x.Fields)
						hasRows = true
						clientOut.Send(m)
					case *pgproto3.DataRow:
						wireRows = append(wireRows, encodeWireRow(x.Values))
						clientOut.Send(m)
					case *pgproto3.CommandComplete:
						cmdTag = string(x.CommandTag)
						clientOut.Send(m)
					case *pgproto3.EmptyQueryResponse:
						clientOut.Send(m)
					case *pgproto3.ErrorResponse:
						errStr = formatErrorResponse(x)
						clientOut.Send(m)
					case *pgproto3.ReadyForQuery:
						rfqMsg = m
						break simpleLoop
					default:
						clientOut.Send(m)
					}
				}
				if isCopy {
					if errStr == "" {
						_, normSQL, _, _ := sqlClassify(op.sql)
						if sink := resolveWriteSink(opt, op.sql, nil); sink != "" {
							emitWriteOutput(context.Background(), kv, sink, op.sql, normSQL, nil, cmdTag, parseCopyTagCount(cmdTag), nil)
						}
					}
				} else if strings.TrimSpace(op.sql) != "" {
					_, normSQL, isWrite, hasReturning := sqlClassify(op.sql)
					if !isTxControlSQL(normSQL) {
						described := hasRows || !isWrite || hasReturning
						recordWireOpResult(kv, opt, op.sql, nil, described, fields, wireRows, cmdTag, errStr)
					}
				}
				if rfqMsg != nil {
					clientOut.Send(rfqMsg)
				}
				if err := clientOut.Flush(); err != nil {
					return
				}
			}
		}
	}()

	// Goroutine 1: Client -> Upstream message decoder.
	stmts := make(map[string]string)
	portals := make(map[string]*wirePortal)
	for {
		msg, err := beParser.Receive()
		if err != nil {
			break
		}
		switch m := msg.(type) {
		case *pgproto3.Parse:
			stmts[m.Name] = m.Query
			opCh <- wireOp{kind: wireOpParse}
		case *pgproto3.Bind:
			sqlStr := stmts[m.PreparedStatement]
			args := formatWireParams(m.Parameters, m.ParameterFormatCodes)
			if opt.ArgNormalizer != nil {
				args = opt.ArgNormalizer(sqlStr, args)
			}
			portals[m.DestinationPortal] = &wirePortal{sql: sqlStr, args: args}
			opCh <- wireOp{kind: wireOpBind}
		case *pgproto3.Describe:
			if m.ObjectType == 'P' {
				if p := portals[m.Name]; p != nil {
					p.described = true
				}
				opCh <- wireOp{kind: wireOpDescribePortal}
			} else {
				opCh <- wireOp{kind: wireOpDescribeStmt, sql: stmts[m.Name]}
			}
		case *pgproto3.Execute:
			var sqlStr string
			var args []string
			var described bool
			if p := portals[m.Portal]; p != nil {
				sqlStr = p.sql
				args = append([]string(nil), p.args...)
				described = p.described
			}
			opCh <- wireOp{kind: wireOpExecute, sql: sqlStr, args: args, described: described}
		case *pgproto3.Close:
			if m.ObjectType == 'S' {
				delete(stmts, m.Name)
			} else if m.ObjectType == 'P' {
				delete(portals, m.Name)
			}
			opCh <- wireOp{kind: wireOpClose}
		case *pgproto3.Sync:
			opCh <- wireOp{kind: wireOpSync}
		case *pgproto3.Query:
			opCh <- wireOp{kind: wireOpSimpleQuery, sql: m.String}
		case *pgproto3.Terminate:
			close(opCh)
			wg.Wait()
			return
		}
	}
	close(opCh)
	_ = upstream.Close()
	wg.Wait()
}

func recordWireGateway(kv *kavach.Kavach, sqlStr string, args []string, rs *RecordedResultSet, errStr string) {
	ctx := context.Background()
	call, _, _, _ := buildQueryGatewayCall(sqlStr, args)
	if errStr != "" {
		call.Error = errStr
	} else if rs != nil {
		if encoded, err := marshalResultSet(rs); err == nil {
			call.Response = encoded
		}
	}
	kv.RecordGateway(ctx, call)
}

func recordWireDescribeGateway(kv *kavach.Kavach, sqlStr string, rs *RecordedResultSet, errStr string) {
	ctx := context.Background()
	_, normSQL, _, _ := sqlClassify(sqlStr)
	call := kavach.GatewayCall{
		Driver:       "pg",
		Gateway:      "pg:describe",
		Op:           sqlStr,
		NormalizedOp: normSQL,
	}
	if errStr != "" {
		call.Error = errStr
	} else if rs != nil {
		if encoded, err := marshalResultSet(rs); err == nil {
			call.Response = encoded
		}
	}
	kv.RecordGateway(ctx, call)
}

func recordWireOpResult(
	kv *kavach.Kavach,
	opt WireOptions,
	sqlStr string,
	args []string,
	described bool,
	fields []pgproto3.FieldDescription,
	wireRows [][]*string,
	cmdTag string,
	errStr string,
) {
	ctx := context.Background()
	_, normSQL, isWrite, hasReturning := sqlClassify(sqlStr)
	if isTxControlSQL(normSQL) {
		return
	}

	if described && !(isWrite && !hasReturning && len(fields) == 0) {
		rs := buildWireResultSet(fields, wireRows, cmdTag, sqlStr)
		recordWireGateway(kv, sqlStr, args, rs, errStr)
		if isWrite && hasReturning && errStr == "" {
			if sink := resolveWriteSink(opt, sqlStr, args); sink != "" {
				emitWriteOutput(ctx, kv, sink, sqlStr, normSQL, args, rs.CommandTag, int64(len(wireRows)), nil)
			}
		}
		return
	}

	if isWrite && errStr == "" {
		if sink := resolveWriteSink(opt, sqlStr, args); sink != "" {
			emitWriteOutput(ctx, kv, sink, sqlStr, normSQL, args, defaultCommandTag(sqlStr), 0, nil)
		}
	}
}

func buildWireResultSet(fields []pgproto3.FieldDescription, wireRows [][]*string, cmdTag, sqlStr string) *RecordedResultSet {
	cols := make([]string, len(fields))
	oids := make([]uint32, len(fields))
	fmts := make([]int16, len(fields))
	for i, fd := range fields {
		cols[i] = string(fd.Name)
		oids[i] = fd.DataTypeOID
		fmts[i] = fd.Format
	}
	if cmdTag == "" {
		cmdTag = defaultCommandTag(sqlStr)
	}
	return &RecordedResultSet{
		Columns:       cols,
		ColumnOIDs:    oids,
		ColumnFormats: fmts,
		WireRows:      wireRows,
		CommandTag:    cmdTag,
		RowsAffected:  int64(len(wireRows)),
	}
}

// --- Replay Mode: 100% Offline In-Process pgproto3 Backend ---

type replayPortal struct {
	sql       string
	args      []string
	described bool
	rs        *RecordedResultSet
}

func serveReplayWireConn(conn net.Conn, kv *kavach.Kavach, opt WireOptions) {
	defer conn.Close()
	be := pgproto3.NewBackend(conn, conn)

	// 1. Handle Postgres v3 startup handshake.
	for {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		case *pgproto3.CancelRequest:
			return
		case *pgproto3.StartupMessage:
			be.Send(&pgproto3.AuthenticationOk{})
			be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
			be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
			be.Send(&pgproto3.ParameterStatus{Name: "DateStyle", Value: "ISO, MDY"})
			be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
			be.Send(&pgproto3.ParameterStatus{Name: "TimeZone", Value: "UTC"})
			be.Send(&pgproto3.ParameterStatus{Name: "integer_datetimes", Value: "on"})
			be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := be.Flush(); err != nil {
				return
			}
		default:
			return
		}
		break
	}

	// 2. Serve extended, simple, and COPY queries completely offline.
	txStatus := byte('I')
	stmts := make(map[string]string)
	portals := make(map[string]*replayPortal)
	skipUntilSync := false

	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}

		if skipUntilSync {
			switch msg.(type) {
			case *pgproto3.Sync:
				skipUntilSync = false
				be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
				if err := be.Flush(); err != nil {
					return
				}
			case *pgproto3.Terminate:
				return
			}
			continue
		}

		switch m := msg.(type) {
		case *pgproto3.Terminate:
			return

		case *pgproto3.Flush:
			if err := be.Flush(); err != nil {
				return
			}

		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
			if err := be.Flush(); err != nil {
				return
			}

		case *pgproto3.Parse:
			stmts[m.Name] = m.Query
			be.Send(&pgproto3.ParseComplete{})

		case *pgproto3.Bind:
			sqlStr := stmts[m.PreparedStatement]
			args := formatWireParams(m.Parameters, m.ParameterFormatCodes)
			if opt.ArgNormalizer != nil {
				args = opt.ArgNormalizer(sqlStr, args)
			}
			portals[m.DestinationPortal] = &replayPortal{
				sql:  sqlStr,
				args: args,
			}
			be.Send(&pgproto3.BindComplete{})

		case *pgproto3.Close:
			if m.ObjectType == 'S' {
				delete(stmts, m.Name)
			} else if m.ObjectType == 'P' {
				delete(portals, m.Name)
			}
			be.Send(&pgproto3.CloseComplete{})

		case *pgproto3.Describe:
			if m.ObjectType == 'S' {
				sqlStr := stmts[m.Name]
				be.Send(&pgproto3.ParameterDescription{})
				if rs := replayWireDescribe(kv, sqlStr); rs != nil && len(rs.Columns) > 0 {
					be.Send(buildRowDescription(rs))
				} else {
					be.Send(&pgproto3.NoData{})
				}
				continue
			}
			p := portals[m.Name]
			if p == nil {
				be.Send(&pgproto3.NoData{})
				continue
			}
			_, normSQL, isWrite, hasReturning := sqlClassify(p.sql)
			if isTxControlSQL(normSQL) || (isWrite && !hasReturning) {
				// Pure DML inside SendBatch or tx-control: no result columns; handled on Execute as Output/tx-control.
				be.Send(&pgproto3.NoData{})
				continue
			}
			p.described = true
			rs, qErr := replayWireQuery(kv, p.sql, p.args)
			if qErr != nil {
				be.Send(buildErrorResponse(qErr))
				if txStatus == 'T' {
					txStatus = 'E'
				}
				skipUntilSync = true
				continue
			}
			p.rs = rs
			if len(rs.Columns) > 0 {
				be.Send(buildRowDescription(rs))
			} else {
				be.Send(&pgproto3.NoData{})
			}

		case *pgproto3.Execute:
			p := portals[m.Portal]
			if p == nil {
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
				continue
			}
			_, normSQL, isWrite, hasReturning := sqlClassify(p.sql)
			if p.described {
				rs := p.rs
				if rs == nil {
					rs = &RecordedResultSet{CommandTag: defaultCommandTag(p.sql)}
				}
				sendWireRows(be, rs)
				tag := rs.CommandTag
				if tag == "" {
					tag = defaultCommandTag(p.sql)
				}
				rowCount := int64(len(rs.WireRows))
				if rowCount == 0 {
					rowCount = int64(len(rs.Rows))
				}
				if isWrite && hasReturning && !isTxControlSQL(normSQL) {
					if _, ok := kv.ActiveStep(context.Background()); ok {
						if sink := resolveWriteSink(opt, p.sql, p.args); sink != "" {
							emitWriteOutput(context.Background(), kv, sink, p.sql, normSQL, p.args, tag, rowCount, nil)
						}
					}
				}
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
				continue
			}

			// Non-Describe Execute (pgx.Exec):
			if isTxControlSQL(normSQL) {
				txStatus = nextTxStatus(txStatus, normSQL)
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte(txControlTag(normSQL))})
				continue
			}
			if isWrite {
				tag := defaultCommandTag(p.sql)
				if _, ok := kv.ActiveStep(context.Background()); ok {
					if sink := resolveWriteSink(opt, p.sql, p.args); sink != "" {
						emitWriteOutput(context.Background(), kv, sink, p.sql, normSQL, p.args, tag, 0, nil)
					}
				}
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
				continue
			}
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})

		case *pgproto3.Query:
			sqlStr := m.String
			trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sqlStr), ";"))
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				be.Send(&pgproto3.EmptyQueryResponse{})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
				if err := be.Flush(); err != nil {
					return
				}
				continue
			}

			upper := strings.ToUpper(trimmed)
			if strings.HasPrefix(upper, "COPY ") && strings.Contains(upper, " FROM STDIN") {
				if err := handleReplayCopyIn(be, kv, opt, sqlStr, &txStatus); err != nil {
					return
				}
				continue
			}

			_, normSQL, isWrite, hasReturning := sqlClassify(sqlStr)
			if isTxControlSQL(normSQL) {
				txStatus = nextTxStatus(txStatus, normSQL)
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte(txControlTag(normSQL))})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
				if err := be.Flush(); err != nil {
					return
				}
				continue
			}

			if !isWrite || hasReturning {
				rs, qErr := replayWireQuery(kv, sqlStr, nil)
				if qErr != nil {
					be.Send(buildErrorResponse(qErr))
					if txStatus == 'T' {
						txStatus = 'E'
					}
					be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
					if err := be.Flush(); err != nil {
						return
					}
					continue
				}
				if len(rs.Columns) > 0 {
					be.Send(buildRowDescription(rs))
					sendWireRows(be, rs)
				}
				tag := rs.CommandTag
				if tag == "" {
					tag = defaultCommandTag(sqlStr)
				}
				if isWrite && hasReturning {
					rowCount := int64(len(rs.WireRows))
					if rowCount == 0 {
						rowCount = int64(len(rs.Rows))
					}
					if _, ok := kv.ActiveStep(context.Background()); ok {
						if sink := resolveWriteSink(opt, sqlStr, nil); sink != "" {
							emitWriteOutput(context.Background(), kv, sink, sqlStr, normSQL, nil, tag, rowCount, nil)
						}
					}
				}
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
				if err := be.Flush(); err != nil {
					return
				}
				continue
			}

			// Simple-protocol write without RETURNING.
			tag := defaultCommandTag(sqlStr)
			if _, ok := kv.ActiveStep(context.Background()); ok {
				if sink := resolveWriteSink(opt, sqlStr, nil); sink != "" {
					emitWriteOutput(context.Background(), kv, sink, sqlStr, normSQL, nil, tag, 0, nil)
				}
			}
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
			if err := be.Flush(); err != nil {
				return
			}
		}
	}
}

func replayWireQuery(kv *kavach.Kavach, sqlStr string, args []string) (*RecordedResultSet, error) {
	ctx := context.Background()
	if _, ok := kv.ActiveStep(ctx); !ok {
		return &RecordedResultSet{CommandTag: defaultCommandTag(sqlStr)}, nil
	}
	call, _, _, _ := buildQueryGatewayCall(sqlStr, args)
	respBytes, err := kv.ReplayGateway(ctx, call)
	if err != nil {
		return nil, reconstructPGError(err)
	}
	rs, decErr := unmarshalResultSet(respBytes)
	if decErr != nil {
		return nil, decErr
	}
	if rs.Error != "" {
		return nil, reconstructPGError(errors.New(rs.Error))
	}
	return rs, nil
}

func replayWireDescribe(kv *kavach.Kavach, sqlStr string) *RecordedResultSet {
	ctx := context.Background()
	if _, ok := kv.ActiveStep(ctx); ok {
		_, normSQL, _, _ := sqlClassify(sqlStr)
		call := kavach.GatewayCall{
			Driver:       "pg",
			Gateway:      "pg:describe",
			Op:           sqlStr,
			NormalizedOp: normSQL,
		}
		if respBytes, err := kv.ReplayGateway(ctx, call); err == nil {
			if rs, decErr := unmarshalResultSet(respBytes); decErr == nil {
				return rs
			}
		}
	}
	return synthesizeSelectDescribe(sqlStr)
}

func synthesizeSelectDescribe(sqlStr string) *RecordedResultSet {
	trimmed := strings.TrimSpace(sqlStr)
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT ") {
		return nil
	}
	fromIdx := strings.Index(upper, " FROM ")
	if fromIdx <= len("SELECT ") {
		return nil
	}
	colsPart := trimmed[len("SELECT "):fromIdx]
	rawCols := strings.Split(colsPart, ",")
	cols := make([]string, 0, len(rawCols))
	oids := make([]uint32, 0, len(rawCols))
	fmts := make([]int16, 0, len(rawCols))
	for _, rc := range rawCols {
		c := strings.Trim(strings.TrimSpace(rc), `"`)
		if c == "" || c == "*" {
			continue
		}
		cols = append(cols, c)
		oids = append(oids, pgtype.TextOID)
		fmts = append(fmts, pgproto3.TextFormat)
	}
	if len(cols) == 0 {
		return nil
	}
	return &RecordedResultSet{Columns: cols, ColumnOIDs: oids, ColumnFormats: fmts, CommandTag: "DESCRIBE"}
}

func handleReplayCopyIn(be *pgproto3.Backend, kv *kavach.Kavach, opt WireOptions, sqlStr string, txStatus *byte) error {
	isBinary := strings.Contains(strings.ToUpper(sqlStr), "BINARY")
	colCount := countCopyColumns(sqlStr)
	overall := byte(0)
	colFmt := uint16(0)
	if isBinary {
		overall = 1
		colFmt = 1
	}
	fmts := make([]uint16, colCount)
	for i := range fmts {
		fmts[i] = colFmt
	}
	be.Send(&pgproto3.CopyInResponse{OverallFormat: overall, ColumnFormatCodes: fmts})
	if err := be.Flush(); err != nil {
		return err
	}

	var copyBuf []byte
	var textLines int64
	for {
		msg, err := be.Receive()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.CopyData:
			if isBinary {
				copyBuf = append(copyBuf, m.Data...)
			} else {
				for _, b := range m.Data {
					if b == '\n' {
						textLines++
					}
				}
			}
		case *pgproto3.CopyDone:
			rowCount := textLines
			if isBinary {
				rowCount = countBinaryCopyRows(copyBuf)
			}
			tag := fmt.Sprintf("COPY %d", rowCount)
			_, normSQL, _, _ := sqlClassify(sqlStr)
			if _, ok := kv.ActiveStep(context.Background()); ok {
				if sink := resolveWriteSink(opt, sqlStr, nil); sink != "" {
					emitWriteOutput(context.Background(), kv, sink, sqlStr, normSQL, nil, tag, rowCount, nil)
				}
			}
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: *txStatus})
			return be.Flush()
		case *pgproto3.CopyFail:
			be.Send(&pgproto3.ErrorResponse{
				Severity: "ERROR",
				Code:     "57014",
				Message:  m.Message,
			})
			if *txStatus == 'T' {
				*txStatus = 'E'
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: *txStatus})
			return be.Flush()
		}
	}
}

// --- Wire Encoding / Decoding Helpers ---

func formatWireParams(params [][]byte, fmts []int16) []string {
	if len(params) == 0 {
		return nil
	}
	out := make([]string, len(params))
	for i, p := range params {
		if p == nil {
			out[i] = "<nil>"
			continue
		}
		var f int16
		if len(fmts) == 1 {
			f = fmts[0]
		} else if i < len(fmts) {
			f = fmts[i]
		}
		if f == pgproto3.BinaryFormat {
			out[i] = "hex:" + hex.EncodeToString(p)
		} else {
			out[i] = string(p)
		}
	}
	return out
}

func cloneFieldDescriptions(in []pgproto3.FieldDescription) []pgproto3.FieldDescription {
	out := make([]pgproto3.FieldDescription, len(in))
	for i, fd := range in {
		cp := fd
		cp.Name = append([]byte(nil), fd.Name...)
		out[i] = cp
	}
	return out
}

func encodeWireRow(vals [][]byte) []*string {
	row := make([]*string, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		s := base64.StdEncoding.EncodeToString(v)
		row[i] = &s
	}
	return row
}

func buildRowDescription(rs *RecordedResultSet) *pgproto3.RowDescription {
	fields := make([]pgproto3.FieldDescription, len(rs.Columns))
	for i, name := range rs.Columns {
		oid := uint32(pgtype.TextOID)
		if i < len(rs.ColumnOIDs) && rs.ColumnOIDs[i] != 0 {
			oid = rs.ColumnOIDs[i]
		} else if len(rs.Rows) > 0 && i < len(rs.Rows[0]) {
			oid = inferOIDFromCell(rs.Rows[0][i])
		}
		var format int16
		if i < len(rs.ColumnFormats) {
			format = rs.ColumnFormats[i]
		}
		fields[i] = pgproto3.FieldDescription{
			Name:         []byte(name),
			DataTypeOID:  oid,
			DataTypeSize: -1,
			TypeModifier: -1,
			Format:       format,
		}
	}
	return &pgproto3.RowDescription{Fields: fields}
}

func sendWireRows(be *pgproto3.Backend, rs *RecordedResultSet) {
	if len(rs.WireRows) > 0 {
		for _, wr := range rs.WireRows {
			vals := make([][]byte, len(wr))
			for i, cell := range wr {
				if cell == nil {
					vals[i] = nil
					continue
				}
				b, err := base64.StdEncoding.DecodeString(*cell)
				if err != nil {
					vals[i] = []byte(*cell)
				} else {
					vals[i] = b
				}
			}
			be.Send(&pgproto3.DataRow{Values: vals})
		}
		return
	}
	// Backward-compatible fallback: convert TypedCell rows to text-format DataRows.
	for _, row := range rs.Rows {
		vals := make([][]byte, len(row))
		for i, c := range row {
			v, err := decodeCell(c)
			if err != nil || v == nil {
				vals[i] = nil
				continue
			}
			switch x := v.(type) {
			case []byte:
				vals[i] = x
			case time.Time:
				vals[i] = []byte(x.UTC().Format("2006-01-02 15:04:05.999999+00"))
			case bool:
				if x {
					vals[i] = []byte("t")
				} else {
					vals[i] = []byte("f")
				}
			default:
				vals[i] = []byte(fmt.Sprint(x))
			}
		}
		be.Send(&pgproto3.DataRow{Values: vals})
	}
}

func inferOIDFromCell(c TypedCell) uint32 {
	switch c.Type {
	case "bool":
		return pgtype.BoolOID
	case "int64":
		return pgtype.Int8OID
	case "float64":
		return pgtype.Float8OID
	case "bytes":
		return pgtype.ByteaOID
	case "time":
		return pgtype.TimestamptzOID
	default:
		return pgtype.TextOID
	}
}

func formatErrorResponse(er *pgproto3.ErrorResponse) string {
	sev := er.Severity
	if sev == "" {
		sev = "ERROR"
	}
	if er.Code != "" {
		return fmt.Sprintf("%s: %s (SQLSTATE %s)", sev, er.Message, er.Code)
	}
	return fmt.Sprintf("%s: %s", sev, er.Message)
}

func buildErrorResponse(err error) *pgproto3.ErrorResponse {
	pgErr := reconstructPGError(err)
	var pe *pgconn.PgError
	if errors.As(pgErr, &pe) {
		sev := pe.Severity
		if sev == "" {
			sev = "ERROR"
		}
		return &pgproto3.ErrorResponse{
			Severity: sev,
			Code:     pe.Code,
			Message:  pe.Message,
		}
	}
	return &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "XX000",
		Message:  err.Error(),
	}
}

func nextTxStatus(cur byte, normSQL string) byte {
	fields := strings.Fields(strings.TrimSpace(normSQL))
	if len(fields) == 0 {
		return cur
	}
	switch strings.ToUpper(fields[0]) {
	case "BEGIN", "START":
		return 'T'
	case "COMMIT", "END":
		return 'I'
	case "ROLLBACK":
		// ROLLBACK TO SAVEPOINT keeps the outer transaction active.
		if len(fields) >= 2 && strings.ToUpper(fields[1]) == "TO" {
			return 'T'
		}
		return 'I'
	default:
		return cur
	}
}

func txControlTag(normSQL string) string {
	fields := strings.Fields(strings.TrimSpace(normSQL))
	if len(fields) == 0 {
		return "OK"
	}
	return strings.ToUpper(fields[0])
}

func countCopyColumns(sqlStr string) int {
	l := strings.IndexByte(sqlStr, '(')
	r := strings.IndexByte(sqlStr, ')')
	if l < 0 || r <= l {
		return 1
	}
	parts := strings.Split(sqlStr[l+1:r], ",")
	if len(parts) == 0 {
		return 1
	}
	return len(parts)
}

func parseCopyTagCount(tag string) int64 {
	fields := strings.Fields(tag)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "COPY") {
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// countBinaryCopyRows parses a PostgreSQL binary COPY stream ("PGCOPY\n\xff\r\n\x00")
// and returns the exact number of tuples written.
func countBinaryCopyRows(buf []byte) int64 {
	const headerMin = 19
	if len(buf) < headerMin {
		return 0
	}
	// Skip 11-byte signature + 4-byte flags + 4-byte extension len + extension bytes.
	extLen := int(binary.BigEndian.Uint32(buf[15:19]))
	pos := 19 + extLen
	var count int64
	for pos+2 <= len(buf) {
		ncols := int16(binary.BigEndian.Uint16(buf[pos : pos+2]))
		pos += 2
		if ncols < 0 {
			break // trailer -1
		}
		for c := int16(0); c < ncols; c++ {
			if pos+4 > len(buf) {
				return count
			}
			flen := int32(binary.BigEndian.Uint32(buf[pos : pos+4]))
			pos += 4
			if flen > 0 {
				if pos+int(flen) > len(buf) {
					return count
				}
				pos += int(flen)
			}
		}
		count++
	}
	return count
}

func rawPassthrough(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
	}()
	wg.Wait()
}
