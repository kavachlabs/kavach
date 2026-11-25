package kavachpgx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kavachlabs/kavach-go"
)

// ReplaySQLDB returns an offline *sql.DB backed by Kavach's replay SQL driver.
// It NEVER dials Postgres or opens any network socket, and plugs directly into
// database/sql or gorm.io/driver/postgres.New(postgres.Config{Conn: sqlDB}).
func ReplaySQLDB(kv *kavach.Kavach) *sql.DB {
	return sql.OpenDB(&kavachConnector{kv: kv})
}

// OpenDBFromPool returns a Kavach-wrapped *sql.DB from a *pgxpool.Pool, mirroring
// github.com/jackc/pgx/v5/stdlib.OpenDBFromPool:
//   - ModeReplay: returns an offline *sql.DB without touching pool (pool may be nil).
//   - ModeRecord: wraps stdlib.GetPoolConnector(pool, opts...) to record SQL Gateway/Output.
//   - ModeOff:    delegates directly to stdlib.OpenDBFromPool(pool, opts...).
func OpenDBFromPool(kv *kavach.Kavach, pool *pgxpool.Pool, opts ...stdlib.OptionOpenDB) *sql.DB {
	if kv == nil || kv.IsOff() {
		return stdlib.OpenDBFromPool(pool, opts...)
	}
	if kv.IsReplay() {
		return ReplaySQLDB(kv)
	}
	inner := stdlib.GetPoolConnector(pool, opts...)
	db := sql.OpenDB(&kavachConnector{kv: kv, inner: inner})
	db.SetMaxIdleConns(0)
	return db
}

// OpenSQL opens a *pgxpool.Pool and returns a Kavach-wrapped *sql.DB + pool handle.
// In ModeReplay, returns (ReplaySQLDB(kv), nil, nil) immediately with ZERO Postgres connection.
func OpenSQL(ctx context.Context, kv *kavach.Kavach, dsn string, opts ...stdlib.OptionOpenDB) (*sql.DB, *pgxpool.Pool, error) {
	if kv != nil && kv.IsReplay() {
		return ReplaySQLDB(kv), nil, nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, err
	}
	return OpenSQLWithConfig(ctx, kv, cfg, opts...)
}

// OpenSQLWithConfig opens a *pgxpool.Pool from cfg and returns a Kavach-wrapped *sql.DB + pool.
// In ModeReplay, returns (ReplaySQLDB(kv), nil, nil) immediately with ZERO Postgres connection.
func OpenSQLWithConfig(ctx context.Context, kv *kavach.Kavach, cfg *pgxpool.Config, opts ...stdlib.OptionOpenDB) (*sql.DB, *pgxpool.Pool, error) {
	if kv != nil && kv.IsReplay() {
		return ReplaySQLDB(kv), nil, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return OpenDBFromPool(kv, pool, opts...), pool, nil
}

// WrapConnector wraps any database/sql/driver.Connector with Kavach record/replay.
// In ModeReplay, inner is never connected to.
func WrapConnector(kv *kavach.Kavach, inner driver.Connector) driver.Connector {
	if kv == nil || kv.IsOff() {
		return inner
	}
	return &kavachConnector{kv: kv, inner: inner}
}

// WrapSQLDriver wraps any database/sql/driver.Driver with Kavach record/replay.
// In ModeReplay, inner is never opened or dialed.
func WrapSQLDriver(kv *kavach.Kavach, inner driver.Driver) driver.Driver {
	if kv == nil || kv.IsOff() {
		return inner
	}
	return &kavachDriver{kv: kv, inner: inner}
}

// WrapSQLDB wraps an existing *sql.DB so queries and execs record in ModeRecord
// and replay 100% offline in ModeReplay (inner may be nil in ModeReplay).
func WrapSQLDB(kv *kavach.Kavach, inner *sql.DB) *sql.DB {
	if kv == nil || kv.IsOff() {
		return inner
	}
	if kv.IsReplay() {
		return ReplaySQLDB(kv)
	}
	return sql.OpenDB(&sqlDBWrapperConnector{kv: kv, inner: inner})
}

// RegisterSQLDriver registers a Kavach-wrapped database/sql driver under name.
func RegisterSQLDriver(name string, kv *kavach.Kavach, inner driver.Driver) {
	sql.Register(name, WrapSQLDriver(kv, inner))
}

type kavachDriver struct {
	kv    *kavach.Kavach
	inner driver.Driver
}

func (d *kavachDriver) Open(name string) (driver.Conn, error) {
	if d.kv != nil && d.kv.IsReplay() {
		return &kavachConn{kv: d.kv}, nil
	}
	if d.inner == nil {
		return nil, errors.New("kavachpgx: nil underlying sql driver")
	}
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &kavachConn{kv: d.kv, inner: c}, nil
}

type kavachConnector struct {
	kv    *kavach.Kavach
	inner driver.Connector
}

func (c *kavachConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.kv != nil && c.kv.IsReplay() {
		return &kavachConn{kv: c.kv}, nil
	}
	if c.inner == nil {
		return nil, errors.New("kavachpgx: nil underlying sql connector")
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &kavachConn{kv: c.kv, inner: conn}, nil
}

func (c *kavachConnector) Driver() driver.Driver {
	var innerDrv driver.Driver
	if c.inner != nil {
		innerDrv = c.inner.Driver()
	}
	return &kavachDriver{kv: c.kv, inner: innerDrv}
}

type kavachConn struct {
	kv    *kavach.Kavach
	inner driver.Conn
}

var (
	_ driver.Conn               = (*kavachConn)(nil)
	_ driver.ConnBeginTx        = (*kavachConn)(nil)
	_ driver.ConnPrepareContext = (*kavachConn)(nil)
	_ driver.Pinger             = (*kavachConn)(nil)
	_ driver.QueryerContext     = (*kavachConn)(nil)
	_ driver.ExecerContext      = (*kavachConn)(nil)
	_ driver.SessionResetter    = (*kavachConn)(nil)
	_ driver.Validator          = (*kavachConn)(nil)
	_ driver.NamedValueChecker  = (*kavachConn)(nil)
)

func (c *kavachConn) CheckNamedValue(nv *driver.NamedValue) error {
	if c.inner != nil {
		if nvc, ok := c.inner.(driver.NamedValueChecker); ok {
			return nvc.CheckNamedValue(nv)
		}
	}
	return driver.ErrSkip
}

func (c *kavachConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *kavachConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if c.kv != nil && c.kv.IsReplay() {
		return &kavachStmt{conn: c, query: query}, nil
	}
	if c.inner == nil {
		return nil, errors.New("kavachpgx: nil underlying sql conn")
	}
	var innerStmt driver.Stmt
	var err error
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		innerStmt, err = pc.PrepareContext(ctx, query)
	} else {
		innerStmt, err = c.inner.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &kavachStmt{conn: c, query: query, inner: innerStmt}, nil
}

func (c *kavachConn) Close() error {
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}

func (c *kavachConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *kavachConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.kv != nil && c.kv.IsReplay() {
		return &noopSQLTx{}, nil
	}
	if c.inner == nil {
		return nil, errors.New("kavachpgx: nil underlying sql conn")
	}
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

func (c *kavachConn) Ping(ctx context.Context) error {
	if c.kv != nil && c.kv.IsReplay() {
		return nil
	}
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *kavachConn) ResetSession(ctx context.Context) error {
	if c.kv != nil && c.kv.IsReplay() {
		return nil
	}
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *kavachConn) IsValid() bool {
	if c.kv != nil && c.kv.IsReplay() {
		return true
	}
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *kavachConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return queryDriver(ctx, c.kv, query, args, func() (driver.Rows, error) {
		if c.inner == nil {
			return nil, errors.New("kavachpgx: nil underlying sql conn")
		}
		if qc, ok := c.inner.(driver.QueryerContext); ok {
			rows, err := qc.QueryContext(ctx, query, args)
			if !errors.Is(err, driver.ErrSkip) {
				return rows, err
			}
		}
		stmt, err := c.inner.Prepare(query)
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		if sqc, ok := stmt.(driver.StmtQueryContext); ok {
			return sqc.QueryContext(ctx, args)
		}
		return stmt.Query(namedToValues(args))
	})
}

func (c *kavachConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return execDriver(ctx, c.kv, query, args, func() (driver.Result, error) {
		if c.inner == nil {
			return nil, errors.New("kavachpgx: nil underlying sql conn")
		}
		if ec, ok := c.inner.(driver.ExecerContext); ok {
			res, err := ec.ExecContext(ctx, query, args)
			if !errors.Is(err, driver.ErrSkip) {
				return res, err
			}
		}
		stmt, err := c.inner.Prepare(query)
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		if sec, ok := stmt.(driver.StmtExecContext); ok {
			return sec.ExecContext(ctx, args)
		}
		return stmt.Exec(namedToValues(args))
	})
}

type kavachStmt struct {
	conn  *kavachConn
	query string
	inner driver.Stmt
}

var (
	_ driver.Stmt              = (*kavachStmt)(nil)
	_ driver.StmtExecContext   = (*kavachStmt)(nil)
	_ driver.StmtQueryContext  = (*kavachStmt)(nil)
	_ driver.NamedValueChecker = (*kavachStmt)(nil)
)

func (s *kavachStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if s.inner != nil {
		if nvc, ok := s.inner.(driver.NamedValueChecker); ok {
			return nvc.CheckNamedValue(nv)
		}
	}
	if s.conn != nil {
		return s.conn.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (s *kavachStmt) Close() error {
	if s.inner != nil {
		return s.inner.Close()
	}
	return nil
}

func (s *kavachStmt) NumInput() int {
	if s.inner != nil {
		return s.inner.NumInput()
	}
	return -1
}

func (s *kavachStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

func (s *kavachStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *kavachStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return execDriver(ctx, s.conn.kv, s.query, args, func() (driver.Result, error) {
		if sec, ok := s.inner.(driver.StmtExecContext); ok {
			return sec.ExecContext(ctx, args)
		}
		return s.inner.Exec(namedToValues(args))
	})
}

func (s *kavachStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return queryDriver(ctx, s.conn.kv, s.query, args, func() (driver.Rows, error) {
		if sqc, ok := s.inner.(driver.StmtQueryContext); ok {
			return sqc.QueryContext(ctx, args)
		}
		return s.inner.Query(namedToValues(args))
	})
}

// sqlDBWrapperConnector allows WrapSQLDB to wrap an arbitrary *sql.DB cleanly.
type sqlDBWrapperConnector struct {
	kv    *kavach.Kavach
	inner *sql.DB
}

func (c *sqlDBWrapperConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &sqlDBWrapperConn{kv: c.kv, inner: c.inner}, nil
}

func (c *sqlDBWrapperConnector) Driver() driver.Driver {
	return &kavachDriver{kv: c.kv}
}

type sqlDBWrapperConn struct {
	kv    *kavach.Kavach
	inner *sql.DB
}

func (c *sqlDBWrapperConn) Prepare(query string) (driver.Stmt, error) {
	return &sqlDBWrapperStmt{conn: c, query: query}, nil
}
func (c *sqlDBWrapperConn) Close() error                       { return nil }
func (c *sqlDBWrapperConn) Begin() (driver.Tx, error)          { return &noopSQLTx{}, nil }
func (c *sqlDBWrapperConn) Ping(ctx context.Context) error     { return c.inner.PingContext(ctx) }
func (c *sqlDBWrapperConn) IsValid() bool                      { return true }
func (c *sqlDBWrapperConn) ResetSession(context.Context) error { return nil }

func (c *sqlDBWrapperConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return queryDriver(ctx, c.kv, query, args, func() (driver.Rows, error) {
		anyArgs := namedToAny(args)
		rows, err := c.inner.QueryContext(ctx, query, anyArgs...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		rs, err := materializeStdSQLRows(rows)
		if err != nil {
			return nil, err
		}
		return newMemDriverRows(rs), nil
	})
}

func (c *sqlDBWrapperConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return execDriver(ctx, c.kv, query, args, func() (driver.Result, error) {
		res, err := c.inner.ExecContext(ctx, query, namedToAny(args)...)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		return driver.RowsAffected(affected), nil
	})
}

type sqlDBWrapperStmt struct {
	conn  *sqlDBWrapperConn
	query string
}

func (s *sqlDBWrapperStmt) Close() error  { return nil }
func (s *sqlDBWrapperStmt) NumInput() int { return -1 }
func (s *sqlDBWrapperStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.ExecContext(context.Background(), s.query, valuesToNamed(args))
}
func (s *sqlDBWrapperStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.QueryContext(context.Background(), s.query, valuesToNamed(args))
}
func (s *sqlDBWrapperStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}
func (s *sqlDBWrapperStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

func queryDriver(
	ctx context.Context,
	kv *kavach.Kavach,
	query string,
	args []driver.NamedValue,
	liveFn func() (driver.Rows, error),
) (driver.Rows, error) {
	if kv == nil || kv.IsOff() {
		return liveFn()
	}
	argStrs := formatNamedArgs(args)
	call, normSQL, isWrite, hasReturning := buildQueryGatewayCall(query, argStrs)

	if kv.IsReplay() {
		if _, ok := kv.ActiveStep(ctx); !ok {
			return newMemDriverRows(&RecordedResultSet{}), nil
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
				tag = defaultCommandTag(query)
			}
			emitExecOutput(ctx, kv, query, normSQL, argStrs, tag, int64(len(rs.Rows)), nil)
		}
		return newMemDriverRows(rs), nil
	}

	// Record mode: execute real query, materialize rows, record Gateway, return in-memory rows.
	rows, err := liveFn()
	if err != nil {
		call.Error = err.Error()
		kv.RecordGateway(ctx, call)
		return nil, err
	}
	var rs *RecordedResultSet
	if mr, ok := rows.(*memDriverRows); ok {
		rs = mr.rs
	} else {
		var matErr error
		rs, matErr = materializeDriverRows(rows)
		if matErr != nil {
			call.Error = matErr.Error()
			kv.RecordGateway(ctx, call)
			return nil, matErr
		}
	}
	if rs.CommandTag == "" {
		rs.CommandTag = defaultCommandTag(query)
	}
	encoded, _ := marshalResultSet(rs)
	call.Response = encoded
	kv.RecordGateway(ctx, call)
	if isWrite && hasReturning {
		emitExecOutput(ctx, kv, query, normSQL, argStrs, rs.CommandTag, int64(len(rs.Rows)), nil)
	}
	return newMemDriverRows(rs), nil
}

func execDriver(
	ctx context.Context,
	kv *kavach.Kavach,
	query string,
	args []driver.NamedValue,
	liveFn func() (driver.Result, error),
) (driver.Result, error) {
	if kv == nil || kv.IsOff() {
		return liveFn()
	}
	argStrs := formatNamedArgs(args)
	_, normSQL, _, _ := sqlClassify(query)
	txCtrl := isTxControlSQL(normSQL)

	if kv.IsReplay() {
		if txCtrl {
			return driver.RowsAffected(0), nil
		}
		tagStr := defaultCommandTag(query)
		if _, ok := kv.ActiveStep(ctx); ok {
			emitExecOutput(ctx, kv, query, normSQL, argStrs, tagStr, 0, nil)
		}
		return driver.RowsAffected(1), nil
	}

	res, err := liveFn()
	if !txCtrl {
		emitExecOutput(ctx, kv, query, normSQL, argStrs, defaultCommandTag(query), 0, err)
	}
	return res, err
}

func materializeDriverRows(rows driver.Rows) (*RecordedResultSet, error) {
	defer rows.Close()
	cols := append([]string(nil), rows.Columns()...)
	var outRows [][]TypedCell
	dest := make([]driver.Value, len(cols))
	for {
		err := rows.Next(dest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		cells := make([]TypedCell, len(dest))
		for i, v := range dest {
			cells[i] = encodeCell(v)
		}
		outRows = append(outRows, cells)
	}
	return &RecordedResultSet{
		Columns: cols,
		Rows:    outRows,
	}, nil
}

func materializeStdSQLRows(rows *sql.Rows) (*RecordedResultSet, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var outRows [][]TypedCell
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		for i := range raw {
			raw[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		cells := make([]TypedCell, len(raw))
		for i, v := range raw {
			cells[i] = encodeCell(v)
		}
		outRows = append(outRows, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &RecordedResultSet{
		Columns: cols,
		Rows:    outRows,
	}, nil
}

type memDriverRows struct {
	rs  *RecordedResultSet
	idx int
}

func newMemDriverRows(rs *RecordedResultSet) *memDriverRows {
	if rs == nil {
		rs = &RecordedResultSet{}
	}
	return &memDriverRows{rs: rs}
}

func (r *memDriverRows) Columns() []string { return r.rs.Columns }
func (r *memDriverRows) Close() error      { return nil }

func (r *memDriverRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rs.Rows) {
		return io.EOF
	}
	row := r.rs.Rows[r.idx]
	r.idx++
	for i := 0; i < len(dest) && i < len(row); i++ {
		v, err := decodeCell(row[i])
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

type noopSQLTx struct{}

func (*noopSQLTx) Commit() error   { return nil }
func (*noopSQLTx) Rollback() error { return nil }

func namedToValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

func namedToAny(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}
