package kavachpgx_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachpgx"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

// customStatusEnum mirrors Hatchet sqlcv1 custom string enums implementing sql.Scanner.
type customStatusEnum string

func (e *customStatusEnum) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*e = customStatusEnum(v)
	case []byte:
		*e = customStatusEnum(string(v))
	default:
		return fmt.Errorf("unsupported enum src %T", src)
	}
	return nil
}

// inMemPGServer is a minimal in-process pgproto3 server for testing
// NewPGXPoolWithConfig in ModeRecord without requiring a live Postgres daemon.
type inMemPGServer struct {
	mu       sync.Mutex
	queries  []string
	copyRows int
}

func (s *inMemPGServer) dial(_ context.Context, _, _ string) (net.Conn, error) {
	c, srv := net.Pipe()
	go s.serve(srv)
	return c, nil
}

func (s *inMemPGServer) serve(conn net.Conn) {
	defer conn.Close()
	be := pgproto3.NewBackend(conn, conn)

	msg, err := be.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, ok := msg.(*pgproto3.StartupMessage); !ok {
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	be.Send(&pgproto3.ParameterStatus{Name: "DateStyle", Value: "ISO, MDY"})
	be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	be.Send(&pgproto3.ParameterStatus{Name: "TimeZone", Value: "UTC"})
	be.Send(&pgproto3.ParameterStatus{Name: "integer_datetimes", Value: "on"})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return
	}

	stmts := map[string]string{}
	portals := map[string]string{}
	described := map[string]bool{}
	txStatus := byte('I')

	for {
		m, err := be.Receive()
		if err != nil {
			return
		}
		switch x := m.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Parse:
			stmts[x.Name] = x.Query
			be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			portals[x.DestinationPortal] = stmts[x.PreparedStatement]
			described[x.DestinationPortal] = false
			be.Send(&pgproto3.BindComplete{})
		case *pgproto3.Close:
			be.Send(&pgproto3.CloseComplete{})
		case *pgproto3.Describe:
			if x.ObjectType == 'S' {
				sqlStr := stmts[x.Name]
				be.Send(&pgproto3.ParameterDescription{})
				if strings.Contains(strings.ToLower(sqlStr), "queue_items") {
					be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
						{Name: []byte("id"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
						{Name: []byte("queue"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
					}})
				} else {
					be.Send(&pgproto3.NoData{})
				}
				continue
			}
			described[x.Name] = true
			sqlStr := portals[x.Name]
			if strings.Contains(sqlStr, "FROM hatchet_tasks") {
				be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("id"), DataTypeOID: pgtype.UUIDOID, DataTypeSize: 16, Format: pgproto3.TextFormat},
					{Name: []byte("parent_id"), DataTypeOID: pgtype.UUIDOID, DataTypeSize: 16, Format: pgproto3.TextFormat},
					{Name: []byte("nil_uuid"), DataTypeOID: pgtype.UUIDOID, DataTypeSize: 16, Format: pgproto3.TextFormat},
					{Name: []byte("child_ids"), DataTypeOID: pgtype.UUIDArrayOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
					{Name: []byte("created_at"), DataTypeOID: pgtype.TimestamptzOID, DataTypeSize: 8, Format: pgproto3.TextFormat},
					{Name: []byte("scheduled_at"), DataTypeOID: pgtype.TimestampOID, DataTypeSize: 8, Format: pgproto3.TextFormat},
					{Name: []byte("name"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
					{Name: []byte("priority"), DataTypeOID: pgtype.Int4OID, DataTypeSize: 4, Format: pgproto3.TextFormat},
					{Name: []byte("seq"), DataTypeOID: pgtype.Int8OID, DataTypeSize: 8, Format: pgproto3.TextFormat},
					{Name: []byte("active"), DataTypeOID: pgtype.BoolOID, DataTypeSize: 1, Format: pgproto3.TextFormat},
					{Name: []byte("payload"), DataTypeOID: pgtype.JSONBOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
					{Name: []byte("status"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
				}})
			} else if strings.Contains(sqlStr, "INSERT INTO task_batches") {
				be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("batch_id"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, Format: pgproto3.TextFormat},
				}})
			} else {
				be.Send(&pgproto3.NoData{})
			}
		case *pgproto3.Execute:
			sqlStr := portals[x.Portal]
			s.mu.Lock()
			s.queries = append(s.queries, sqlStr)
			s.mu.Unlock()
			switch {
			case strings.Contains(sqlStr, "FROM hatchet_tasks"):
				be.Send(&pgproto3.DataRow{Values: [][]byte{
					[]byte("11111111-1111-1111-1111-111111111111"),
					[]byte("22222222-2222-2222-2222-222222222222"),
					nil,
					[]byte("{33333333-3333-3333-3333-333333333333,44444444-4444-4444-4444-444444444444}"),
					[]byte("2026-01-15 10:30:00.123456+00"),
					[]byte("2026-01-15 10:31:00.000000"),
					[]byte("step-run-1"),
					[]byte("3"),
					[]byte("9000000001"),
					[]byte("t"),
					[]byte(`{"k":"v"}`),
					[]byte("RUNNING"),
				}})
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			case strings.Contains(sqlStr, "INSERT INTO task_batches"):
				be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("batch-fixed-1")}})
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
			case strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqlStr)), "UPDATE"):
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")})
			case strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqlStr)), "SELECT"):
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
			default:
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
			}
		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
			if err := be.Flush(); err != nil {
				return
			}
		case *pgproto3.Query:
			q := strings.TrimSpace(x.String)
			upper := strings.ToUpper(q)
			switch {
			case q == "" || q == ";":
				be.Send(&pgproto3.EmptyQueryResponse{})
			case strings.HasPrefix(upper, "BEGIN"):
				txStatus = 'T'
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
			case strings.HasPrefix(upper, "COMMIT"):
				txStatus = 'I'
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")})
			case strings.HasPrefix(upper, "ROLLBACK"):
				txStatus = 'I'
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
			case strings.HasPrefix(upper, "COPY ") && strings.Contains(upper, "FROM STDIN"):
				be.Send(&pgproto3.CopyInResponse{OverallFormat: 1, ColumnFormatCodes: []uint16{1, 1}})
				if err := be.Flush(); err != nil {
					return
				}
				for {
					cm, cerr := be.Receive()
					if cerr != nil {
						return
					}
					if _, done := cm.(*pgproto3.CopyDone); done {
						break
					}
				}
				s.mu.Lock()
				s.copyRows = 2
				s.mu.Unlock()
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("COPY 2")})
			default:
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
			if err := be.Flush(); err != nil {
				return
			}
		}
	}
}

func TestPGXWirePoolFullTypesBatchCopyAndPG40001(t *testing.T) {
	ringPath := filepath.Join(t.TempDir(), "hatchet-wire.shm")
	recKV := kavach.Init(kavach.Config{
		Service:  "hatchet",
		RingPath: ringPath,
		Mode:     kavach.ModeRecord,
	})

	wireOpts := kavachpgx.WireOptions{
		ArgNormalizer: func(sqlStr string, args []string) []string {
			if strings.Contains(sqlStr, "task_batches") && len(args) > 0 {
				cp := append([]string(nil), args...)
				cp[0] = "<normalized-batch-id>"
				return cp
			}
			return args
		},
		WriteSink: func(sqlStr string, _ []string) string {
			if strings.Contains(sqlStr, "worker_leases") {
				return "pg:lease:worker"
			}
			if strings.Contains(sqlStr, "ignore_me") {
				return "" // suppressed
			}
			return "pg:exec"
		},
	}

	srv := &inMemPGServer{}
	recCfg, err := pgxpool.ParseConfig("postgres://user@127.0.0.1:5432/hatchet?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	recCfg.ConnConfig.DialFunc = srv.dial

	recPool, err := kavachpgx.NewPGXPoolWithConfig(context.Background(), recKV, recCfg, wireOpts)
	if err != nil {
		t.Fatalf("NewPGXPoolWithConfig record: %v", err)
	}
	defer recPool.Close()

	runQueries := func(ctx context.Context, pool *pgxpool.Pool, randomBatchID string) error {
		// 1. SELECT scanning all Hatchet Postgres types (UUID, *UUID, []UUID, Timestamptz, Timestamp, Text, Int4, Int8, Bool, JSONB, custom enum).
		var (
			id          [16]byte
			parentID    *[16]byte
			nilUUID     *pgtype.UUID
			childIDs    []pgtype.UUID
			createdAt   pgtype.Timestamptz
			scheduledAt pgtype.Timestamp
			name        pgtype.Text
			priority    pgtype.Int4
			seq         pgtype.Int8
			active      bool
			payload     []byte
			status      customStatusEnum
		)
		if err := pool.QueryRow(ctx,
			`SELECT id, parent_id, nil_uuid, child_ids, created_at, scheduled_at, name, priority, seq, active, payload, status FROM hatchet_tasks WHERE tenant_id = $1`,
			"tenant-1",
		).Scan(&id, &parentID, &nilUUID, &childIDs, &createdAt, &scheduledAt, &name, &priority, &seq, &active, &payload, &status); err != nil {
			return fmt.Errorf("scan hatchet_tasks: %w", err)
		}
		if parentID == nil || nilUUID != nil || len(childIDs) != 2 || !createdAt.Valid || !scheduledAt.Valid {
			return fmt.Errorf("unexpected uuid/time scan: parent=%v nil=%v children=%d created=%v sched=%v",
				parentID, nilUUID, len(childIDs), createdAt.Valid, scheduledAt.Valid)
		}
		if name.String != "step-run-1" || priority.Int32 != 3 || seq.Int64 != 9000000001 || !active || string(payload) != `{"k":"v"}` || status != "RUNNING" {
			return fmt.Errorf("unexpected scalar scan: name=%q prio=%d seq=%d active=%v payload=%s status=%q",
				name.String, priority.Int32, seq.Int64, active, payload, status)
		}

		// 2. BeginTx + INSERT ... RETURNING with volatile batch_id normalized via ArgNormalizer + Commit.
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("BeginTx: %w", err)
		}
		var batchOut string
		if err := tx.QueryRow(ctx, `INSERT INTO task_batches (batch_id) VALUES ($1) RETURNING batch_id`, randomBatchID).Scan(&batchOut); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("INSERT RETURNING: %w", err)
		}
		if batchOut != "batch-fixed-1" {
			return fmt.Errorf("batchOut=%q", batchOut)
		}
		// Non-Describe SELECT Exec (advisory lock style).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(42)); err != nil {
			return fmt.Errorf("advisory lock exec: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("Commit: %w", err)
		}

		// 3. SendBatch with custom WriteSink ("pg:lease:worker") and suppressed WriteSink ("").
		batch := &pgx.Batch{}
		batch.Queue(`UPDATE worker_leases SET heartbeat = $1 WHERE id = $2`, "now", "w-1")
		batch.Queue(`UPDATE ignore_me SET x = $1`, 1)
		br := pool.SendBatch(ctx, batch)
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch exec 1: %w", err)
		}
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch exec 2: %w", err)
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("batch close: %w", err)
		}

		// 4. CopyFrom with 2 rows.
		n, err := pool.CopyFrom(ctx, pgx.Identifier{"queue_items"}, []string{"id", "queue"}, pgx.CopyFromRows([][]any{
			{"qi-1", "default"},
			{"qi-2", "default"},
		}))
		if err != nil {
			return fmt.Errorf("CopyFrom: %w", err)
		}
		if n != 2 {
			return fmt.Errorf("CopyFrom count=%d, want 2", n)
		}
		return nil
	}

	stepCtx, sc := recKV.BeginStep(context.Background(), kavach.Input{Source: "hatchet:step", Data: []byte(`{"step":1}`)})
	if err := runQueries(stepCtx, recPool, "rand-uuid-record-111"); err != nil {
		t.Fatalf("record runQueries: %v", err)
	}
	if err := sc.End(nil); err != nil {
		t.Fatalf("end record step: %v", err)
	}
	_ = recKV.Close()

	// Decode recorded KVRING02 ring.
	recs := decodeRingRecords(t, ringPath)
	var gws []kavach.GatewayCall
	var outs []matcher.OutputRecord
	for _, r := range recs {
		switch r.Type {
		case ring.RecordGateway:
			gc := matcher.ParseGatewayRequest(r.Gateway, r.Request)
			gc.Response = r.Response
			gc.Error = r.Error
			gws = append(gws, gc)
		case ring.RecordOutput:
			outs = append(outs, matcher.OutputRecord{Sink: r.Sink, Data: r.Data})
		}
	}
	// Gateways: SELECT hatchet_tasks, INSERT RETURNING task_batches, SELECT pg_advisory_xact_lock, plus pg:describe for CopyFrom.
	if len(gws) != 4 {
		t.Fatalf("expected 4 Gateway records (SELECT + INSERT RETURNING + advisory lock + CopyFrom describe), got %d: %+v", len(gws), gws)
	}
	// Outputs: INSERT RETURNING ("pg:exec"), UPDATE worker_leases ("pg:lease:worker"), COPY ("pg:exec").
	// (ignore_me suppressed, BEGIN/COMMIT/advisory SELECT skipped!)
	if len(outs) != 3 {
		t.Fatalf("expected 3 outputs, got %d: %+v", len(outs), outs)
	}
	if outs[1].Sink != "pg:lease:worker" {
		t.Fatalf("expected custom WriteSink pg:lease:worker, got %q", outs[1].Sink)
	}

	// Replay mode: 100% offline, unreachable DSN, DIFFERENT randomBatchID!
	replayKV := kavach.Init(kavach.Config{Service: "hatchet", Mode: kavach.ModeReplay})
	replayPool, err := kavachpgx.NewPGXPool(context.Background(), replayKV, "postgres://bad:bad@192.0.2.1:5432/nope?sslmode=require", wireOpts)
	if err != nil {
		t.Fatalf("NewPGXPool replay: %v", err)
	}
	defer replayPool.Close()

	// Startup Ping outside any step must succeed offline!
	if err := replayPool.Ping(context.Background()); err != nil {
		t.Fatalf("replayPool.Ping failed: %v", err)
	}

	tbl := matcher.NewStepTable(1, gws, outs)
	_, err = replayKV.RunStepWithTable(context.Background(), kavach.Input{Source: "hatchet:step", Data: []byte(`{"step":1}`)}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		return runQueries(ctx, replayPool, "rand-uuid-replay-999")
	}))
	if err != nil {
		t.Fatalf("offline wire replay failed: %v", err)
	}
	if err := tbl.CompareOutputs(); err != nil {
		t.Fatalf("wire record-vs-replay outputs diverged: %v", err)
	}

	// Inject PG_40001 on wire Replay -> pgx/v5 must receive a real *pgconn.PgError{Code: "40001"}!
	tblFault := matcher.NewStepTable(2, gws, nil)
	tblFault.InjectFault(kavach.FaultRule{
		Driver:    "pg",
		Gateway:   "pg:select",
		ErrString: kavach.CodePG40001,
		Once:      true,
	})
	_, faultErr := replayKV.RunStepWithTable(context.Background(), kavach.Input{Source: "hatchet:step"}, tblFault, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		var dummy string
		return replayPool.QueryRow(ctx,
			`SELECT id, parent_id, nil_uuid, child_ids, created_at, scheduled_at, name, priority, seq, active, payload, status FROM hatchet_tasks WHERE tenant_id = $1`,
			"tenant-1",
		).Scan(&dummy)
	}))
	var pgErr *pgconn.PgError
	if !errors.As(faultErr, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("expected wire *pgconn.PgError 40001, got %T (%v)", faultErr, faultErr)
	}
}

func TestHighLevelPoolDBTXSendBatchAndCopyFrom(t *testing.T) {
	kv := kavach.Init(kavach.Config{Service: "hatchet-dbtx", Mode: kavach.ModeReplay})
	pool := kavachpgx.Wrap(kv, nil)

	// Compile-time check: *kavachpgx.Pool satisfies kavachpgx.DB (sqlc DBTX + Begin/Ping/Close).
	var _ kavachpgx.DB = pool

	rsBytes, _ := json.Marshal(kavachpgx.RecordedResultSet{
		Columns: []string{"cnt"},
		Rows:    [][]kavachpgx.TypedCell{{{Type: "int64", Val: "7"}}},
	})
	tbl := matcher.NewStepTable(1, []kavach.GatewayCall{{
		Driver:       "pg",
		Gateway:      "pg:select",
		Op:           "SELECT count(*) FROM tasks",
		NormalizedOp: matcher.NormalizeSQL("SELECT count(*) FROM tasks"),
		Response:     rsBytes,
	}}, nil)

	sc, err := kv.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:batch"}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		b := &pgx.Batch{}
		var gotCnt int64
		b.Queue("SELECT count(*) FROM tasks").QueryRow(func(row pgx.Row) error {
			return row.Scan(&gotCnt)
		})
		b.Queue("UPDATE tasks SET state = $1 WHERE id = $2", "DONE", "t-1")
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		if gotCnt != 7 {
			return fmt.Errorf("batch gotCnt=%d, want 7", gotCnt)
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"items"}, []string{"id"}, pgx.CopyFromRows([][]any{{"i1"}, {"i2"}, {"i3"}}))
		if err != nil || n != 3 {
			return fmt.Errorf("tx.CopyFrom = %d, %v", n, err)
		}
		return tx.Commit(ctx)
	}))
	if err != nil {
		t.Fatalf("DBTX SendBatch/CopyFrom failed: %v", err)
	}
	if len(sc.Outputs()) != 2 {
		t.Fatalf("expected 2 outputs (UPDATE + COPY), got %d: %+v", len(sc.Outputs()), sc.Outputs())
	}
}

// statefulFakeDriver supports NamedValueChecker (*[]byte) and read-your-own-writes
// rollback/commit semantics to test WrapStatefulConnector (Blnk #50).
type statefulFakeDriver struct {
	mu         sync.Mutex
	committed  map[string]int64
	nvcInvoked bool
}

func (d *statefulFakeDriver) Open(string) (driver.Conn, error) {
	return &statefulFakeConn{d: d}, nil
}

type statefulFakeConnector struct{ d *statefulFakeDriver }

func (c *statefulFakeConnector) Connect(context.Context) (driver.Conn, error) { return c.d.Open("") }
func (c *statefulFakeConnector) Driver() driver.Driver                        { return c.d }

type statefulFakeConn struct {
	d     *statefulFakeDriver
	inTx  bool
	draft map[string]int64
}

func (c *statefulFakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *statefulFakeConn) Close() error                        { return nil }

func (c *statefulFakeConn) CheckNamedValue(nv *driver.NamedValue) error {
	c.d.mu.Lock()
	c.d.nvcInvoked = true
	c.d.mu.Unlock()
	if bp, ok := nv.Value.(*[]byte); ok && bp != nil {
		nv.Value = *bp
		return nil
	}
	return driver.ErrSkip
}

func (c *statefulFakeConn) Begin() (driver.Tx, error) {
	c.inTx = true
	c.draft = make(map[string]int64)
	c.d.mu.Lock()
	for k, v := range c.d.committed {
		c.draft[k] = v
	}
	c.d.mu.Unlock()
	return &statefulFakeTx{c: c}, nil
}

func (c *statefulFakeConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	key := fmt.Sprint(args[0].Value)
	var val int64
	if c.inTx {
		val = c.draft[key]
	} else {
		c.d.mu.Lock()
		val = c.d.committed[key]
		c.d.mu.Unlock()
	}
	return &fakeRows{cols: []string{"balance"}, rows: [][]driver.Value{{val}}}, nil
}

func (c *statefulFakeConn) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	key := fmt.Sprint(args[0].Value)
	amt, _ := args[1].Value.(int64)
	if c.inTx {
		c.draft[key] = amt
	} else {
		c.d.mu.Lock()
		c.d.committed[key] = amt
		c.d.mu.Unlock()
	}
	return driver.RowsAffected(1), nil
}

type statefulFakeTx struct{ c *statefulFakeConn }

func (t *statefulFakeTx) Commit() error {
	t.c.d.mu.Lock()
	t.c.d.committed = t.c.draft
	t.c.d.mu.Unlock()
	t.c.inTx = false
	t.c.draft = nil
	return nil
}

func (t *statefulFakeTx) Rollback() error {
	t.c.inTx = false
	t.c.draft = nil
	return nil
}

func TestWrapStatefulConnectorCommitRollbackAndNamedValueChecker(t *testing.T) {
	for _, mode := range []kavach.Mode{kavach.ModeRecord, kavach.ModeReplay} {
		t.Run(mode.String(), func(t *testing.T) {
			ringPath := ""
			if mode == kavach.ModeRecord {
				ringPath = filepath.Join(t.TempDir(), "blnk.shm")
			}
			kv := kavach.Init(kavach.Config{Service: "blnk", RingPath: ringPath, Mode: mode})

			drv := &statefulFakeDriver{committed: map[string]int64{"bal-1": 1000}}
			var observed []string
			conn := kavachpgx.WrapStatefulConnector(
				kv,
				&statefulFakeConnector{d: drv},
				kavachpgx.WithSQLArgNormalizer(func(_ string, args []string) []string {
					if len(args) >= 3 {
						cp := append([]string(nil), args...)
						cp[2] = "<meta-normalized>"
						return cp
					}
					return args
				}),
				kavachpgx.WithSQLWriteObserver(func(_ context.Context, query string, args []string, affected int64, err error) bool {
					observed = append(observed, fmt.Sprintf("%s|%v|%d", query, args, affected))
					return true
				}),
			)
			db := sql.OpenDB(conn)
			defer db.Close()

			run := func(ctx context.Context) error {
				// 1. Rolled-back transaction MUST NOT emit pg:exec or invoke WriteObserver!
				tx1, err := db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				meta := []byte(`{"note":"discard"}`)
				if _, err := tx1.ExecContext(ctx, "UPDATE blnk.balances SET balance = $2, meta = $3 WHERE id = $1", "bal-1", int64(9999), &meta); err != nil {
					return err
				}
				if err := tx1.Rollback(); err != nil {
					return err
				}

				// 2. Committed transaction reads its own write & emits pg:exec on Commit!
				tx2, err := db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				meta2 := []byte(`{"note":"commit"}`)
				if _, err := tx2.ExecContext(ctx, "UPDATE blnk.balances SET balance = $2, meta = $3 WHERE id = $1", "bal-1", int64(5000), &meta2); err != nil {
					return err
				}
				var readOwn int64
				if err := tx2.QueryRowContext(ctx, "SELECT balance FROM blnk.balances WHERE id = $1", "bal-1").Scan(&readOwn); err != nil {
					return err
				}
				if readOwn != 5000 {
					return fmt.Errorf("read-your-own-write got %d, want 5000", readOwn)
				}
				return tx2.Commit()
			}

			var sc *kavach.StepContext
			var err error
			if mode == kavach.ModeRecord {
				var stepCtx context.Context
				stepCtx, sc = kv.BeginStep(context.Background(), kavach.Input{Source: "http:POST /transactions", Data: []byte(`{}`)})
				err = run(stepCtx)
				_ = sc.End(err)
				_ = kv.Close()
			} else {
				tbl := matcher.NewStepTable(1, nil, nil) // 0 Gateway records!
				sc, err = kv.RunStepWithTable(context.Background(), kavach.Input{Source: "http:POST /transactions", Data: []byte(`{}`)}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
					return run(ctx)
				}))
			}
			if err != nil {
				t.Fatalf("mode %s failed: %v", mode, err)
			}
			if !drv.nvcInvoked {
				t.Fatalf("expected NamedValueChecker to be forwarded for *[]byte")
			}
			if len(observed) != 1 || !strings.Contains(observed[0], "<meta-normalized>") {
				t.Fatalf("expected 1 committed write observed (rollback discarded), got %v", observed)
			}
			if len(sc.Outputs()) != 1 || sc.Outputs()[0].Sink != "pg:exec" {
				t.Fatalf("expected 1 committed pg:exec output, got %+v", sc.Outputs())
			}
			if mode == kavach.ModeRecord {
				recs := decodeRingRecords(t, ringPath)
				for _, r := range recs {
					if r.Type == ring.RecordGateway {
						t.Fatalf("WrapStatefulConnector must not emit TypeGateway for SELECT: %+v", r)
					}
				}
				_ = os.Remove(ringPath)
			}
		})
	}
}

var _ io.Reader = (*strings.Reader)(nil)
