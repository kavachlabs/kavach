package kavachpgx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kavachlabs/kavach-go"
)

// DB is the common query/exec/transaction interface satisfied by both
// *pgxpool.Pool, *pgx.Conn, sqlc DBTX, and *kavachpgx.Pool.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Ping(ctx context.Context) error
	Close()
}

// Pool wraps a *pgxpool.Pool (or runs completely offline in ModeReplay when pool is nil).
type Pool struct {
	kv   *kavach.Kavach
	pool *pgxpool.Pool
}

var _ DB = (*Pool)(nil)

// Wrap wraps an existing *pgxpool.Pool with Kavach record/replay interception.
// In ModeReplay, pool may be nil and is never dialed or queried.
func Wrap(kv *kavach.Kavach, pool *pgxpool.Pool) *Pool {
	return &Pool{kv: kv, pool: pool}
}

// NewPool creates a wrapped pgxpool.Pool. In ModeReplay it returns an offline
// *Pool immediately without parsing/dialing dsn or opening any network socket.
func NewPool(ctx context.Context, kv *kavach.Kavach, dsn string) (*Pool, error) {
	if kv != nil && kv.IsReplay() {
		return &Pool{kv: kv}, nil
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Pool{kv: kv, pool: pool}, nil
}

// NewPoolWithConfig creates a wrapped pgxpool.Pool from cfg. In ModeReplay it
// returns an offline *Pool immediately without touching Postgres.
func NewPoolWithConfig(ctx context.Context, kv *kavach.Kavach, cfg *pgxpool.Config) (*Pool, error) {
	if kv != nil && kv.IsReplay() {
		return &Pool{kv: kv}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Pool{kv: kv, pool: pool}, nil
}

// Raw returns the underlying *pgxpool.Pool (nil in offline ModeReplay).
func (p *Pool) Raw() *pgxpool.Pool { return p.pool }

// OpenSQL returns a *sql.DB backed by this Pool (offline in ModeReplay), suitable
// for database/sql or gorm.io/driver/postgres.Config{Conn: sqlDB}.
func (p *Pool) OpenSQL(opts ...stdlib.OptionOpenDB) *sql.DB {
	return OpenDBFromPool(p.kv, p.pool, opts...)
}

// Close closes the underlying pool if present.
func (p *Pool) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

// Ping verifies database connectivity. In ModeReplay it succeeds immediately offline.
func (p *Pool) Ping(ctx context.Context) error {
	if p.kv != nil && p.kv.IsReplay() {
		return nil
	}
	if p.pool == nil {
		return errors.New("kavachpgx: nil underlying pgxpool.Pool")
	}
	return p.pool.Ping(ctx)
}

// Query executes a SQL query (SELECT or RETURNING).
// In ModeRecord, results are captured as a Gateway call (plus Output for mutating RETURNING).
// In ModeReplay, serves recorded rows or injected PG_40001 error completely offline.
func (p *Pool) Query(ctx context.Context, sqlStr string, args ...any) (pgx.Rows, error) {
	return queryPGX(ctx, p.kv, sqlStr, args, func() (pgx.Rows, error) {
		if p.pool == nil {
			return nil, errors.New("kavachpgx: nil underlying pgxpool.Pool")
		}
		return p.pool.Query(ctx, sqlStr, args...)
	})
}

// QueryRow executes a SQL query expected to return at most one row.
func (p *Pool) QueryRow(ctx context.Context, sqlStr string, args ...any) pgx.Row {
	rows, err := p.Query(ctx, sqlStr, args...)
	if err != nil {
		return &errorRow{err: err}
	}
	return &singleRow{rows: rows}
}

// Exec executes a SQL write statement (INSERT/UPDATE/DELETE).
// Recorded and replayed as an Output ("pg:exec") without touching Postgres in ModeReplay.
func (p *Pool) Exec(ctx context.Context, sqlStr string, args ...any) (pgconn.CommandTag, error) {
	return execPGX(ctx, p.kv, sqlStr, args, func() (pgconn.CommandTag, error) {
		if p.pool == nil {
			return pgconn.CommandTag{}, errors.New("kavachpgx: nil underlying pgxpool.Pool")
		}
		return p.pool.Exec(ctx, sqlStr, args...)
	})
}

// CopyFrom implements sqlc DBTX / pgxpool.Pool.CopyFrom.
// In ModeReplay it drains rowSrc offline and emits a "pg:exec" COPY Output.
func (p *Pool) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return copyFromPGX(ctx, p.kv, tableName, columnNames, rowSrc, func() (int64, error) {
		if p.pool == nil {
			return 0, errors.New("kavachpgx: nil underlying pgxpool.Pool")
		}
		return p.pool.CopyFrom(ctx, tableName, columnNames, rowSrc)
	})
}

// SendBatch implements sqlc DBTX / pgxpool.Pool.SendBatch.
func (p *Pool) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if (p.kv == nil || p.kv.IsOff()) && p.pool != nil {
		return p.pool.SendBatch(ctx, b)
	}
	return &wrappedBatchResults{
		ctx:     ctx,
		batch:   b,
		execFn:  p.Exec,
		queryFn: p.Query,
	}
}

// Begin starts a transaction (offline no-op tx in ModeReplay).
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.BeginTx(ctx, pgx.TxOptions{})
}

// BeginTx starts a transaction with txOptions (offline no-op tx in ModeReplay).
func (p *Pool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	if p.kv != nil && p.kv.IsReplay() {
		return &wrappedTx{kv: p.kv}, nil
	}
	if p.pool == nil {
		return nil, errors.New("kavachpgx: nil underlying pgxpool.Pool")
	}
	tx, err := p.pool.BeginTx(ctx, txOptions)
	if err != nil {
		return nil, err
	}
	return &wrappedTx{kv: p.kv, raw: tx}, nil
}

func queryPGX(
	ctx context.Context,
	kv *kavach.Kavach,
	sqlStr string,
	args []any,
	liveFn func() (pgx.Rows, error),
) (pgx.Rows, error) {
	if kv == nil || kv.IsOff() {
		return liveFn()
	}
	argStrs := formatAnyArgs(args)
	call, normSQL, isWrite, hasReturning := buildQueryGatewayCall(sqlStr, argStrs)

	if kv.IsReplay() {
		if _, ok := kv.ActiveStep(ctx); !ok {
			// Startup probe outside a step: return empty rows offline.
			return newReplayRows(&RecordedResultSet{CommandTag: defaultCommandTag(sqlStr)}), nil
		}
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
		if isWrite && hasReturning {
			tag := rs.CommandTag
			if tag == "" {
				tag = defaultCommandTag(sqlStr)
			}
			emitExecOutput(ctx, kv, sqlStr, normSQL, argStrs, tag, int64(len(rs.Rows)), nil)
		}
		return newReplayRows(rs), nil
	}

	// Record mode: run live query, materialize rows into RecordedResultSet, record Gateway.
	rows, err := liveFn()
	if err != nil {
		call.Error = err.Error()
		kv.RecordGateway(ctx, call)
		return nil, err
	}
	rs, matErr := materializePGXRows(rows)
	if matErr != nil {
		call.Error = matErr.Error()
		kv.RecordGateway(ctx, call)
		return nil, matErr
	}
	if rs.CommandTag == "" {
		rs.CommandTag = defaultCommandTag(sqlStr)
	}
	encoded, _ := marshalResultSet(rs)
	call.Response = encoded
	kv.RecordGateway(ctx, call)
	if isWrite && hasReturning {
		emitExecOutput(ctx, kv, sqlStr, normSQL, argStrs, rs.CommandTag, int64(len(rs.Rows)), nil)
	}
	return newReplayRows(rs), nil
}

func execPGX(
	ctx context.Context,
	kv *kavach.Kavach,
	sqlStr string,
	args []any,
	liveFn func() (pgconn.CommandTag, error),
) (pgconn.CommandTag, error) {
	if kv == nil || kv.IsOff() {
		return liveFn()
	}
	argStrs := formatAnyArgs(args)
	_, normSQL, _, _ := sqlClassify(sqlStr)
	txCtrl := isTxControlSQL(normSQL)

	if kv.IsReplay() {
		tagStr := defaultCommandTag(sqlStr)
		if !txCtrl {
			if _, ok := kv.ActiveStep(ctx); ok {
				emitExecOutput(ctx, kv, sqlStr, normSQL, argStrs, tagStr, 0, nil)
			}
		}
		return pgconn.NewCommandTag(tagStr), nil
	}

	tag, err := liveFn()
	if !txCtrl {
		emitExecOutput(ctx, kv, sqlStr, normSQL, argStrs, defaultCommandTag(sqlStr), 0, err)
	}
	return tag, err
}

func emitExecOutput(ctx context.Context, kv *kavach.Kavach, sqlStr, normSQL string, args []string, tag string, affected int64, err error) {
	emitWriteOutput(ctx, kv, "pg:exec", sqlStr, normSQL, args, tag, affected, err)
}

func emitWriteOutput(ctx context.Context, kv *kavach.Kavach, sink, sqlStr, normSQL string, args []string, tag string, affected int64, err error) {
	if kv == nil || sink == "" {
		return
	}
	rec := RecordedExec{
		SQL:           sqlStr,
		NormalizedSQL: normSQL,
		Args:          args,
		CommandTag:    tag,
		RowsAffected:  affected,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	b, _ := json.Marshal(rec)
	kv.RecordOutput(ctx, kavach.Output{
		Sink: sink,
		Key:  normSQL,
		Data: b,
	})
}

func copyFromPGX(
	ctx context.Context,
	kv *kavach.Kavach,
	tableName pgx.Identifier,
	columnNames []string,
	rowSrc pgx.CopyFromSource,
	liveFn func() (int64, error),
) (int64, error) {
	if kv == nil || kv.IsOff() {
		return liveFn()
	}
	copySQL := fmt.Sprintf("COPY %s (%s) FROM STDIN", tableName.Sanitize(), pgx.Identifier(columnNames).Sanitize())
	_, normSQL, _, _ := sqlClassify(copySQL)
	if kv.IsReplay() {
		var count int64
		if rowSrc != nil {
			for rowSrc.Next() {
				if _, err := rowSrc.Values(); err != nil {
					return count, err
				}
				count++
			}
			if err := rowSrc.Err(); err != nil {
				return count, err
			}
		}
		if _, ok := kv.ActiveStep(ctx); ok {
			emitExecOutput(ctx, kv, copySQL, normSQL, nil, fmt.Sprintf("COPY %d", count), count, nil)
		}
		return count, nil
	}
	n, err := liveFn()
	if err == nil {
		emitExecOutput(ctx, kv, copySQL, normSQL, nil, fmt.Sprintf("COPY %d", n), n, nil)
	}
	return n, err
}

func materializePGXRows(rows pgx.Rows) (*RecordedResultSet, error) {
	defer rows.Close()
	fds := rows.FieldDescriptions()
	cols := make([]string, len(fds))
	for i, fd := range fds {
		cols[i] = fd.Name
	}
	var outRows [][]TypedCell
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		cells := make([]TypedCell, len(vals))
		for i, v := range vals {
			cells[i] = encodeCell(v)
		}
		outRows = append(outRows, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tag := rows.CommandTag()
	return &RecordedResultSet{
		Columns:      cols,
		Rows:         outRows,
		CommandTag:   tag.String(),
		RowsAffected: tag.RowsAffected(),
	}, nil
}

// NewTracer returns a pgx.QueryTracer suitable for attaching to pgx.ConnConfig.Tracer
// for lightweight observability when direct Pool/SQL wrapping is not used.
func NewTracer(kv *kavach.Kavach) pgx.QueryTracer {
	return &queryTracer{kv: kv}
}

type tracerKey struct{}

type tracedQuery struct {
	sql  string
	args []string
}

type queryTracer struct {
	kv *kavach.Kavach
}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t.kv == nil || !t.kv.IsRecord() {
		return ctx
	}
	return context.WithValue(ctx, tracerKey{}, tracedQuery{
		sql:  data.SQL,
		args: formatAnyArgs(data.Args),
	})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if t.kv == nil || !t.kv.IsRecord() {
		return
	}
	tq, ok := ctx.Value(tracerKey{}).(tracedQuery)
	if !ok {
		return
	}
	_, normSQL, isWrite, _ := sqlClassify(tq.sql)
	if isWrite {
		emitExecOutput(ctx, t.kv, tq.sql, normSQL, tq.args, data.CommandTag.String(), data.CommandTag.RowsAffected(), data.Err)
	}
}

// replayRows implements pgx.Rows entirely in memory from a RecordedResultSet.
type replayRows struct {
	rs     *RecordedResultSet
	fds    []pgconn.FieldDescription
	idx    int
	closed bool
	err    error
}

var _ pgx.Rows = (*replayRows)(nil)

func newReplayRows(rs *RecordedResultSet) *replayRows {
	if rs == nil {
		rs = &RecordedResultSet{}
	}
	fds := make([]pgconn.FieldDescription, len(rs.Columns))
	for i, name := range rs.Columns {
		fds[i] = pgconn.FieldDescription{Name: name}
	}
	return &replayRows{
		rs:  rs,
		fds: fds,
		idx: -1,
	}
}

func (r *replayRows) Close()     { r.closed = true }
func (r *replayRows) Err() error { return r.err }
func (r *replayRows) CommandTag() pgconn.CommandTag {
	if r.rs.CommandTag != "" {
		return pgconn.NewCommandTag(r.rs.CommandTag)
	}
	return pgconn.NewCommandTag(fmt.Sprintf("SELECT %d", len(r.rs.Rows)))
}
func (r *replayRows) FieldDescriptions() []pgconn.FieldDescription { return r.fds }
func (r *replayRows) Conn() *pgx.Conn                              { return nil }
func (r *replayRows) TypeMap() *pgtype.Map                         { return pgtype.NewMap() }

func (r *replayRows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	r.idx++
	if r.idx >= len(r.rs.Rows) {
		r.closed = true
		return false
	}
	return true
}

func (r *replayRows) Values() ([]any, error) {
	if r.idx < 0 || r.idx >= len(r.rs.Rows) {
		return nil, errors.New("kavachpgx: Values called without active row")
	}
	return decodeRowValues(r.rs.Rows[r.idx])
}

func (r *replayRows) RawValues() [][]byte {
	if r.idx < 0 || r.idx >= len(r.rs.Rows) {
		return nil
	}
	vals, err := decodeRowValues(r.rs.Rows[r.idx])
	if err != nil {
		return nil
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		switch x := v.(type) {
		case []byte:
			out[i] = x
		default:
			out[i] = []byte(fmt.Sprint(x))
		}
	}
	return out
}

func (r *replayRows) Scan(dest ...any) error {
	if r.idx < 0 || r.idx >= len(r.rs.Rows) {
		return errors.New("kavachpgx: Scan called without active row")
	}
	if len(dest) == 1 {
		if rs, ok := dest[0].(pgx.RowScanner); ok {
			return rs.ScanRow(r)
		}
	}
	vals, err := decodeRowValues(r.rs.Rows[r.idx])
	if err != nil {
		r.err = err
		r.closed = true
		return err
	}
	if len(dest) != len(vals) {
		err = fmt.Errorf("kavachpgx: expected %d scan destinations, got %d", len(vals), len(dest))
		r.err = err
		r.closed = true
		return err
	}
	for i := range dest {
		if err := scanInto(vals[i], dest[i]); err != nil {
			r.err = err
			r.closed = true
			return err
		}
	}
	return nil
}

type singleRow struct {
	rows pgx.Rows
}

func (r *singleRow) Scan(dest ...any) error {
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

type errorRow struct {
	err error
}

func (r *errorRow) Scan(_ ...any) error { return r.err }

// wrappedTx implements pgx.Tx in both Record and offline Replay modes.
type wrappedTx struct {
	kv     *kavach.Kavach
	raw    pgx.Tx
	closed bool
}

var _ pgx.Tx = (*wrappedTx)(nil)

func (t *wrappedTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	if t.kv != nil && t.kv.IsReplay() {
		return &wrappedTx{kv: t.kv}, nil
	}
	sub, err := t.raw.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &wrappedTx{kv: t.kv, raw: sub}, nil
}

func (t *wrappedTx) Commit(ctx context.Context) error {
	if t.closed {
		return pgx.ErrTxClosed
	}
	t.closed = true
	if t.kv != nil && t.kv.IsReplay() {
		return nil
	}
	return t.raw.Commit(ctx)
}

func (t *wrappedTx) Rollback(ctx context.Context) error {
	if t.closed {
		return pgx.ErrTxClosed
	}
	t.closed = true
	if t.kv != nil && t.kv.IsReplay() {
		return nil
	}
	return t.raw.Rollback(ctx)
}

func (t *wrappedTx) Exec(ctx context.Context, sqlStr string, arguments ...any) (pgconn.CommandTag, error) {
	if t.closed {
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	return execPGX(ctx, t.kv, sqlStr, arguments, func() (pgconn.CommandTag, error) {
		return t.raw.Exec(ctx, sqlStr, arguments...)
	})
}

func (t *wrappedTx) Query(ctx context.Context, sqlStr string, args ...any) (pgx.Rows, error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	return queryPGX(ctx, t.kv, sqlStr, args, func() (pgx.Rows, error) {
		return t.raw.Query(ctx, sqlStr, args...)
	})
}

func (t *wrappedTx) QueryRow(ctx context.Context, sqlStr string, args ...any) pgx.Row {
	rows, err := t.Query(ctx, sqlStr, args...)
	if err != nil {
		return &errorRow{err: err}
	}
	return &singleRow{rows: rows}
}

func (t *wrappedTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	if t.closed {
		return 0, pgx.ErrTxClosed
	}
	return copyFromPGX(ctx, t.kv, tableName, columnNames, rowSrc, func() (int64, error) {
		if t.raw == nil {
			return 0, errors.New("kavachpgx: nil underlying pgx.Tx")
		}
		return t.raw.CopyFrom(ctx, tableName, columnNames, rowSrc)
	})
}

func (t *wrappedTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if (t.kv == nil || t.kv.IsOff()) && t.raw != nil {
		return t.raw.SendBatch(ctx, b)
	}
	return &wrappedBatchResults{
		ctx:     ctx,
		batch:   b,
		tx:      t,
		execFn:  t.Exec,
		queryFn: t.Query,
	}
}

type wrappedBatchResults struct {
	ctx     context.Context
	batch   *pgx.Batch
	tx      pgx.Tx
	execFn  func(context.Context, string, ...any) (pgconn.CommandTag, error)
	queryFn func(context.Context, string, ...any) (pgx.Rows, error)
	idx     int
	err     error
	closed  bool
}

var _ pgx.BatchResults = (*wrappedBatchResults)(nil)

func (br *wrappedBatchResults) Tx() pgx.Tx { return br.tx }

func (br *wrappedBatchResults) nextQuery() (*pgx.QueuedQuery, error) {
	if br.err != nil {
		return nil, br.err
	}
	if br.batch == nil || br.idx >= len(br.batch.QueuedQueries) {
		return nil, errors.New("kavachpgx: no more results in batch")
	}
	qq := br.batch.QueuedQueries[br.idx]
	br.idx++
	return qq, nil
}

func (br *wrappedBatchResults) Exec() (pgconn.CommandTag, error) {
	qq, err := br.nextQuery()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := br.execFn(br.ctx, qq.SQL, qq.Arguments...)
	if err != nil {
		br.err = err
	}
	return tag, err
}

func (br *wrappedBatchResults) Query() (pgx.Rows, error) {
	qq, err := br.nextQuery()
	if err != nil {
		return &replayRows{err: err, closed: true}, err
	}
	rows, err := br.queryFn(br.ctx, qq.SQL, qq.Arguments...)
	if err != nil {
		br.err = err
		return &replayRows{err: err, closed: true}, err
	}
	return rows, nil
}

func (br *wrappedBatchResults) QueryRow() pgx.Row {
	rows, err := br.Query()
	if err != nil {
		return &errorRow{err: err}
	}
	return &singleRow{rows: rows}
}

func (br *wrappedBatchResults) Close() error {
	if br.closed {
		return br.err
	}
	br.closed = true
	if br.batch == nil {
		return br.err
	}
	for br.err == nil && br.idx < len(br.batch.QueuedQueries) {
		qq := br.batch.QueuedQueries[br.idx]
		if qq.Fn != nil {
			if err := qq.Fn(br); err != nil {
				br.err = err
			}
		} else {
			_, _ = br.Exec()
		}
	}
	return br.err
}

func (t *wrappedTx) LargeObjects() pgx.LargeObjects {
	if t.raw != nil {
		return t.raw.LargeObjects()
	}
	return pgx.LargeObjects{}
}

func (t *wrappedTx) Prepare(ctx context.Context, name, sqlStr string) (*pgconn.StatementDescription, error) {
	if t.kv != nil && t.kv.IsReplay() {
		return &pgconn.StatementDescription{Name: name, SQL: sqlStr}, nil
	}
	return t.raw.Prepare(ctx, name, sqlStr)
}

func (t *wrappedTx) Conn() *pgx.Conn {
	if t.raw != nil {
		return t.raw.Conn()
	}
	return nil
}
