package kavachpgx_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachpgx"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

// fakeSQLDriver is an in-memory database/sql/driver for testing Record mode without Postgres.
type fakeSQLDriver struct {
	onQuery func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error)
	onExec  func(query string, args []driver.NamedValue) (int64, error)
}

func (d *fakeSQLDriver) Open(string) (driver.Conn, error) { return &fakeSQLConn{d: d}, nil }

type fakeSQLConn struct{ d *fakeSQLDriver }

func (c *fakeSQLConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakeSQLConn) Close() error                        { return nil }
func (c *fakeSQLConn) Begin() (driver.Tx, error)           { return &fakeTx{}, nil }

func (c *fakeSQLConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cols, rows, err := c.d.onQuery(query, args)
	if err != nil {
		return nil, err
	}
	return &fakeRows{cols: cols, rows: rows}, nil
}

func (c *fakeSQLConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	n, err := c.d.onExec(query, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(n), nil
}

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	idx  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

type fakeConnector struct{ drv *fakeSQLDriver }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open("") }
func (c *fakeConnector) Driver() driver.Driver                        { return c.drv }

// customStringArray mirrors Lago's utils.StringArray sql.Scanner.
type customStringArray []string

func (a *customStringArray) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*a = nil
		return nil
	case []byte:
		return json.Unmarshal(x, a)
	case string:
		*a = strings.Split(strings.Trim(x, "{}"), ",")
		return nil
	default:
		return errors.New("bad StringArray type")
	}
}

func decodeRingRecords(t *testing.T, path string) []ring.Record {
	t.Helper()
	var raw []byte
	if r, err := ring.Open(path); err == nil {
		defer r.Close()
		raw, err = r.PollOnce()
		if err != nil {
			t.Fatalf("poll ring: %v", err)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read ring: %v", err)
		}
	}
	var dec ring.FrameDecoder
	if _, err := dec.Feed(raw); err != nil {
		t.Fatalf("feed ring: %v", err)
	}
	var out []ring.Record
	for _, f := range dec.FinishEOF().Frames {
		if f.Kind == ring.FrameRecord {
			r, err := ring.DecodeRecord(f.Payload)
			if err != nil {
				t.Fatalf("decode record: %v", err)
			}
			out = append(out, r)
		}
	}
	return out
}

func TestSQLDriverRecordAndOfflineReplayWithContextBackground(t *testing.T) {
	streamPath := filepath.Join(t.TempDir(), "pg.stream")
	recKV := kavach.Init(kavach.Config{
		Service:  "lago-events",
		RingPath: streamPath,
		Mode:     kavach.ModeRecord,
	})

	startedAt := time.Date(2026, 1, 15, 10, 0, 0, 500000, time.UTC)
	liveDriver := &fakeSQLDriver{
		onQuery: func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
			if strings.Contains(query, "FROM subscriptions") {
				return []string{"id", "plan_id", "started_at", "terminated_at", "tags"},
					[][]driver.Value{{
						"sub-1", "plan-pro", startedAt, nil, []byte(`["region","tier"]`),
					}}, nil
			}
			return []string{"id"}, [][]driver.Value{{"ch-advance-1"}}, nil
		},
		onExec: func(query string, args []driver.NamedValue) (int64, error) {
			return 1, nil
		},
	}

	recDB := sql.OpenDB(kavachpgx.WrapConnector(recKV, &fakeConnector{drv: liveDriver}))
	defer recDB.Close()

	// 1. Record mode: mimic Lago GORM calling DB with context.Background() (no step in ctx!).
	stepCtx, sc := recKV.BeginStep(context.Background(), kavach.Input{
		Source: "kafka:events_raw",
		Data:   []byte(`{"transaction_id":"tx-1"}`),
	})
	_ = stepCtx

	var (
		subID, planID string
		st, term      sql.NullTime
		tags          customStringArray
	)
	// Deliberately use QueryRow (uses context.Background() internally, like Lago GORM!).
	if err := recDB.QueryRow(
		"SELECT id, plan_id, started_at, terminated_at, tags FROM subscriptions WHERE organization_id = $1 AND external_id = $2",
		"org-1", "ext-sub-1",
	).Scan(&subID, &planID, &st, &term, &tags); err != nil {
		t.Fatalf("record SELECT subscriptions: %v", err)
	}
	var chargeID string
	if err := recDB.QueryRow(
		"SELECT id FROM charges WHERE organization_id = $1 AND plan_id = $2 AND pay_in_advance IS TRUE LIMIT 1",
		"org-1", planID,
	).Scan(&chargeID); err != nil {
		t.Fatalf("record SELECT charges: %v", err)
	}
	if _, err := recDB.Exec("INSERT INTO audit_log (tx_id, sub_id) VALUES ($1, $2)", "tx-1", subID); err != nil {
		t.Fatalf("record INSERT: %v", err)
	}
	if err := sc.End(nil); err != nil {
		t.Fatalf("end step: %v", err)
	}
	_ = recKV.Close()

	// Verify exact Gateway and Output records were captured.
	recs := decodeRingRecords(t, streamPath)
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
	if len(gws) != 2 {
		t.Fatalf("expected 2 Gateway records (SELECTs), got %d", len(gws))
	}
	if gws[0].Gateway != "pg:select" || gws[1].Gateway != "pg:select" {
		t.Fatalf("expected pg:select gateways, got %q, %q", gws[0].Gateway, gws[1].Gateway)
	}
	if len(outs) != 1 || outs[0].Sink != "pg:exec" {
		t.Fatalf("expected 1 pg:exec Output record, got %+v", outs)
	}

	// 2. Replay mode: ZERO real DB, poison driver, AND reordered SELECTs (charges before subscriptions)!
	poisonDriver := &fakeSQLDriver{
		onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
			t.Fatal("LIVE DB TOUCHED IN REPLAY MODE")
			return nil, nil, nil
		},
		onExec: func(string, []driver.NamedValue) (int64, error) {
			t.Fatal("LIVE DB EXEC IN REPLAY MODE")
			return 0, nil
		},
	}

	replayKV := kavach.Init(kavach.Config{Service: "lago-events", Mode: kavach.ModeReplay})
	replayDB := sql.OpenDB(kavachpgx.WrapConnector(replayKV, &fakeConnector{drv: poisonDriver}))
	defer replayDB.Close()

	// Startup Ping before any step must succeed offline!
	if err := replayDB.Ping(); err != nil {
		t.Fatalf("startup Ping in replay failed: %v", err)
	}

	tbl := matcher.NewStepTable(1, gws, outs)
	sc2, err := replayKV.RunStepWithTable(context.Background(), kavach.Input{
		Source: "kafka:events_raw",
		Data:   []byte(`{"transaction_id":"tx-1"}`),
	}, tbl, kavach.StepFunc(func(_ context.Context, _ kavach.Input) error {
		// Reordered: query charges BEFORE subscriptions, AND still using context.Background()!
		var gotCharge string
		if err := replayDB.QueryRow(
			"SELECT id FROM charges WHERE organization_id = $1 AND plan_id = $2 AND pay_in_advance IS TRUE LIMIT 1",
			"org-1", "plan-pro",
		).Scan(&gotCharge); err != nil {
			return err
		}
		if gotCharge != "ch-advance-1" {
			t.Fatalf("got charge %q, want ch-advance-1", gotCharge)
		}

		var gotSub, gotPlan string
		var gotStart, gotTerm sql.NullTime
		var gotTags customStringArray
		if err := replayDB.QueryRow(
			"SELECT id, plan_id, started_at, terminated_at, tags FROM subscriptions WHERE organization_id = $1 AND external_id = $2",
			"org-1", "ext-sub-1",
		).Scan(&gotSub, &gotPlan, &gotStart, &gotTerm, &gotTags); err != nil {
			return err
		}
		if gotSub != "sub-1" || !gotStart.Valid || !gotStart.Time.Equal(startedAt) || gotTerm.Valid {
			t.Fatalf("unexpected sub scan: id=%q start=%+v term=%+v", gotSub, gotStart, gotTerm)
		}
		if len(gotTags) != 2 || gotTags[0] != "region" || gotTags[1] != "tier" {
			t.Fatalf("unexpected customStringArray: %+v", gotTags)
		}

		res, err := replayDB.Exec("INSERT INTO audit_log (tx_id, sub_id) VALUES ($1, $2)", "tx-1", gotSub)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("expected 1 row affected, got %d", n)
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("reordered offline replay failed: %v", err)
	}
	if tbl.ReorderedCount() == 0 {
		t.Fatalf("expected reordered_gateway count > 0")
	}
	if len(sc2.Outputs()) != 1 || sc2.Outputs()[0].Sink != "pg:exec" {
		t.Fatalf("expected 1 pg:exec output, got %+v", sc2.Outputs())
	}
}

func TestPGXPoolOfflineReplayAndPG40001FaultInjection(t *testing.T) {
	kv := kavach.Init(kavach.Config{Service: "ledger", Mode: kavach.ModeReplay})

	// NewPool with unreachable DSN MUST NOT dial in ModeReplay!
	pool, err := kavachpgx.NewPool(context.Background(), kv, "postgres://invalid:invalid@192.0.2.1:5432/nope")
	if err != nil {
		t.Fatalf("NewPool in replay mode should not fail: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("Pool.Ping in replay mode failed: %v", err)
	}

	rsBytes, _ := json.Marshal(kavachpgx.RecordedResultSet{
		Columns: []string{"balance"},
		Rows:    [][]kavachpgx.TypedCell{{{Type: "int64", Val: "5000"}}},
	})
	recorded := []kavach.GatewayCall{
		{
			Driver:       "pg",
			Gateway:      "pg:select",
			Op:           "SELECT balance FROM accounts WHERE id = $1",
			NormalizedOp: matcher.NormalizeSQL("SELECT balance FROM accounts WHERE id = $1"),
			KeyArgs:      []string{"acct-42"},
			Response:     rsBytes,
		},
	}

	// Handler that retries once on PostgreSQL SQLSTATE 40001 serialization failure.
	handler := kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		var bal int64
		err = tx.QueryRow(ctx, "SELECT balance FROM accounts WHERE id = $1", "acct-42").Scan(&bal)
		if err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
				t.Errorf("expected *pgconn.PgError SQLSTATE 40001, got %T (%v)", err, err)
			}
			return err
		}
		if bal != 5000 {
			t.Errorf("got balance %d, want 5000", bal)
		}
		if _, err := tx.Exec(ctx, "UPDATE accounts SET balance = $1 WHERE id = $2", bal-100, "acct-42"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})

	// Attempt 1: inject PG_40001 -> handler gets genuine *pgconn.PgError{Code: "40001"} and emits 0 writes.
	tbl1 := matcher.NewStepTable(1, recorded, nil)
	tbl1.InjectFault(kavach.FaultRule{
		Driver:    "pg",
		Gateway:   "pg:select",
		ErrString: kavach.CodePG40001,
		Once:      true,
	})
	sc1, err1 := kv.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:debit", Data: []byte(`{"id":"acct-42"}`)}, tbl1, handler)
	var pgErr *pgconn.PgError
	if !errors.As(err1, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("attempt 1 expected PG 40001 error, got %T (%v)", err1, err1)
	}
	if len(sc1.Outputs()) != 0 {
		t.Fatalf("attempt 1 leaked %d outputs on PG_40001 failure", len(sc1.Outputs()))
	}

	// Attempt 2: retry succeeds cleanly and emits 1 pg:exec output.
	tbl2 := matcher.NewStepTable(2, recorded, nil)
	sc2, err2 := kv.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:debit", Data: []byte(`{"id":"acct-42"}`)}, tbl2, handler)
	if err2 != nil {
		t.Fatalf("attempt 2 retry failed: %v", err2)
	}
	if len(sc2.Outputs()) != 1 || sc2.Outputs()[0].Sink != "pg:exec" {
		t.Fatalf("expected 1 pg:exec output on retry, got %+v", sc2.Outputs())
	}

	// Empty result set returns pgx.ErrNoRows cleanly on QueryRow.
	emptyRS, _ := json.Marshal(kavachpgx.RecordedResultSet{Columns: []string{"id"}})
	tbl3 := matcher.NewStepTable(3, []kavach.GatewayCall{{
		Driver:       "pg",
		Gateway:      "pg:select",
		Op:           "SELECT id FROM accounts WHERE id = $1",
		NormalizedOp: matcher.NormalizeSQL("SELECT id FROM accounts WHERE id = $1"),
		KeyArgs:      []string{"missing"},
		Response:     emptyRS,
	}}, nil)
	_, err3 := kv.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:get"}, tbl3, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		var id string
		err := pool.QueryRow(ctx, "SELECT id FROM accounts WHERE id = $1", "missing").Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expected pgx.ErrNoRows, got %v", err)
		}
		return nil
	}))
	if err3 != nil {
		t.Fatalf("ErrNoRows check failed: %v", err3)
	}
}

func TestSQLDriverInsertReturningAndZeroRowUpdateTxControlParity(t *testing.T) {
	// Regression test for Ent-style SQL patterns:
	// 1) INSERT ... RETURNING "id" (including bulk insert returning 2 rows) must emit
	//    byte-for-byte identical pg:exec Output in Record and Replay.
	// 2) UPDATE matching 0 live rows must emit identical pg:exec Output in Record and Replay.
	// 3) Tx-control statements (SAVEPOINT / RELEASE SAVEPOINT) must not emit pg:exec Outputs.
	ringPath := filepath.Join(t.TempDir(), "ent.shm")
	recKV := kavach.Init(kavach.Config{
		Service:  "openmeter",
		RingPath: ringPath,
		Mode:     kavach.ModeRecord,
	})

	liveDriver := &fakeSQLDriver{
		onQuery: func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
			if strings.Contains(query, "INSERT INTO grants") {
				return []string{"id"}, [][]driver.Value{{"gr-1"}, {"gr-2"}}, nil
			}
			return nil, nil, nil
		},
		onExec: func(query string, args []driver.NamedValue) (int64, error) {
			// Simulate 0 rows affected on UPDATE and 0 on SAVEPOINT/RELEASE.
			return 0, nil
		},
	}

	recDB := sql.OpenDB(kavachpgx.WrapConnector(recKV, &fakeConnector{drv: liveDriver}))
	defer recDB.Close()

	runWorkflow := func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, "SAVEPOINT sp_1"); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `INSERT INTO grants (amount) VALUES ($1), ($2) RETURNING "id"`, 100, 200)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) != 2 || ids[0] != "gr-1" || ids[1] != "gr-2" {
			t.Fatalf("unexpected returning ids: %v", ids)
		}
		if _, err := db.ExecContext(ctx, "UPDATE balance_snapshots SET invalidated = $1 WHERE at > $2", true, "2026-01-01"); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "RELEASE SAVEPOINT sp_1"); err != nil {
			return err
		}
		return nil
	}

	stepCtx, sc := recKV.BeginStep(context.Background(), kavach.Input{Source: "cmd:grant", Data: []byte(`{"id":"g1"}`)})
	if err := runWorkflow(stepCtx, recDB); err != nil {
		t.Fatalf("record workflow: %v", err)
	}
	if err := sc.End(nil); err != nil {
		t.Fatalf("end record step: %v", err)
	}
	_ = recKV.Close()

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
	// Expect exactly 1 Gateway (INSERT ... RETURNING) and 2 Outputs (INSERT RETURNING + UPDATE; SAVEPOINT/RELEASE skipped).
	if len(gws) != 1 {
		t.Fatalf("expected 1 Gateway record, got %d", len(gws))
	}
	if len(outs) != 2 {
		t.Fatalf("expected 2 pg:exec outputs (tx-control skipped), got %d: %+v", len(outs), outs)
	}

	// Replay offline and assert byte-for-byte identical Outputs via tbl.CompareOutputs()!
	replayKV := kavach.Init(kavach.Config{Service: "openmeter", Mode: kavach.ModeReplay})
	replayDB := kavachpgx.ReplaySQLDB(replayKV)
	defer replayDB.Close()

	tbl := matcher.NewStepTable(1, gws, outs)
	_, err := replayKV.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:grant", Data: []byte(`{"id":"g1"}`)}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		return runWorkflow(ctx, replayDB)
	}))
	if err != nil {
		t.Fatalf("replay workflow: %v", err)
	}
	if err := tbl.CompareOutputs(); err != nil {
		t.Fatalf("record-vs-replay pg:exec outputs diverged: %v", err)
	}
}
