package kavachpgx

import (
	"context"
	"database/sql/driver"
	"errors"

	"github.com/kavachlabs/kavach-go"
)

// SQLOption configures WrapStatefulConnector.
type SQLOption func(*sqlOptions)

type sqlOptions struct {
	argNormalizer func(query string, args []string) []string
	writeObserver func(ctx context.Context, query string, args []string, affected int64, err error) bool
}

// WithSQLArgNormalizer normalizes SQL argument strings before invoking the write
// observer or emitting a "pg:exec" Output (useful for normalizing generated IDs/timestamps).
func WithSQLArgNormalizer(fn func(query string, args []string) []string) SQLOption {
	return func(o *sqlOptions) {
		o.argNormalizer = fn
	}
}

// WithSQLWriteObserver is invoked for every committed non-tx-control SQL write.
// Returning true emits the default "pg:exec" Output; returning false suppresses
// the default emit (e.g. when the observer emits its own structured Output).
func WithSQLWriteObserver(fn func(ctx context.Context, query string, args []string, affected int64, err error) bool) SQLOption {
	return func(o *sqlOptions) {
		o.writeObserver = fn
	}
}

// WrapStatefulConnector wraps a real local/embedded database/sql driver.Connector
// (such as an in-memory SQLite engine reset on kavach.Config.OnReset) for services
// with read-your-own-writes SQL state semantics:
//   - Executes real QueryContext, ExecContext, PrepareContext, BeginTx/Commit/Rollback
//     against inner in BOTH ModeRecord and ModeReplay.
//   - Does NOT record SELECTs as TypeGateway (so patched builds and mutated variants
//     read their own writes cleanly without UnmatchedGateway errors).
//   - Buffers writes inside a sql.Tx and emits "pg:exec" Outputs (and invokes
//     WithSQLWriteObserver) only on Commit(), discarding them on Rollback().
//   - Forwards driver.NamedValueChecker to inner cleanly.
func WrapStatefulConnector(kv *kavach.Kavach, inner driver.Connector, opts ...SQLOption) driver.Connector {
	if kv == nil || kv.IsOff() {
		return inner
	}
	var cfg sqlOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &statefulConnector{
		kv:    kv,
		inner: inner,
		opts:  cfg,
	}
}

type statefulConnector struct {
	kv    *kavach.Kavach
	inner driver.Connector
	opts  sqlOptions
}

func (c *statefulConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.inner == nil {
		return nil, errors.New("kavachpgx: nil underlying sql connector")
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &statefulConn{kv: c.kv, inner: conn, opts: c.opts}, nil
}

func (c *statefulConnector) Driver() driver.Driver {
	if c.inner != nil {
		return c.inner.Driver()
	}
	return &kavachDriver{kv: c.kv}
}

type pendingSQLWrite struct {
	ctx      context.Context
	query    string
	normSQL  string
	args     []string
	tag      string
	affected int64
	err      error
}

type statefulConn struct {
	kv      *kavach.Kavach
	inner   driver.Conn
	opts    sqlOptions
	inTx    bool
	pending []pendingSQLWrite
}

var (
	_ driver.Conn               = (*statefulConn)(nil)
	_ driver.ConnBeginTx        = (*statefulConn)(nil)
	_ driver.ConnPrepareContext = (*statefulConn)(nil)
	_ driver.Pinger             = (*statefulConn)(nil)
	_ driver.QueryerContext     = (*statefulConn)(nil)
	_ driver.ExecerContext      = (*statefulConn)(nil)
	_ driver.SessionResetter    = (*statefulConn)(nil)
	_ driver.Validator          = (*statefulConn)(nil)
	_ driver.NamedValueChecker  = (*statefulConn)(nil)
)

func (c *statefulConn) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := c.inner.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (c *statefulConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *statefulConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var st driver.Stmt
	var err error
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		st, err = pc.PrepareContext(ctx, query)
	} else {
		st, err = c.inner.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &statefulStmt{conn: c, query: query, inner: st}, nil
}

func (c *statefulConn) Close() error {
	c.inTx = false
	c.pending = nil
	return c.inner.Close()
}

func (c *statefulConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *statefulConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	var err error
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		tx, err = bt.BeginTx(ctx, opts)
	} else {
		tx, err = c.inner.Begin()
	}
	if err != nil {
		return nil, err
	}
	c.inTx = true
	c.pending = c.pending[:0]
	return &statefulTx{conn: c, inner: tx}, nil
}

func (c *statefulConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *statefulConn) ResetSession(ctx context.Context) error {
	c.inTx = false
	c.pending = nil
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *statefulConn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *statefulConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := execInnerQuery(ctx, c.inner, query, args)
	if err == nil {
		_, normSQL, isWrite, hasReturning := sqlClassify(query)
		if isWrite && hasReturning && !isTxControlSQL(normSQL) {
			c.recordWrite(ctx, query, normSQL, formatNamedArgs(args), defaultCommandTag(query), 0, nil)
		}
	}
	return rows, err
}

func (c *statefulConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	res, err := execInnerExec(ctx, c.inner, query, args)
	_, normSQL, _, _ := sqlClassify(query)
	if !isTxControlSQL(normSQL) {
		var affected int64
		if res != nil {
			affected, _ = res.RowsAffected()
		}
		if err == nil {
			c.recordWrite(ctx, query, normSQL, formatNamedArgs(args), defaultCommandTag(query), affected, nil)
		} else if !c.inTx && c.opts.writeObserver != nil {
			argStrs := formatNamedArgs(args)
			if c.opts.argNormalizer != nil {
				argStrs = c.opts.argNormalizer(query, argStrs)
			}
			_ = c.opts.writeObserver(ctx, query, argStrs, affected, err)
		}
	}
	return res, err
}

func (c *statefulConn) recordWrite(ctx context.Context, query, normSQL string, args []string, tag string, affected int64, err error) {
	if c.opts.argNormalizer != nil {
		args = c.opts.argNormalizer(query, args)
	}
	w := pendingSQLWrite{
		ctx:      ctx,
		query:    query,
		normSQL:  normSQL,
		args:     args,
		tag:      tag,
		affected: affected,
		err:      err,
	}
	if c.inTx {
		c.pending = append(c.pending, w)
		return
	}
	c.emitWrite(w)
}

func (c *statefulConn) emitWrite(w pendingSQLWrite) {
	if c.opts.writeObserver != nil {
		if !c.opts.writeObserver(w.ctx, w.query, w.args, w.affected, w.err) {
			return
		}
	}
	emitExecOutput(w.ctx, c.kv, w.query, w.normSQL, w.args, w.tag, 0, w.err)
}

type statefulTx struct {
	conn  *statefulConn
	inner driver.Tx
}

func (t *statefulTx) Commit() error {
	err := t.inner.Commit()
	pending := t.conn.pending
	t.conn.inTx = false
	t.conn.pending = nil
	if err == nil {
		for _, w := range pending {
			t.conn.emitWrite(w)
		}
	}
	return err
}

func (t *statefulTx) Rollback() error {
	t.conn.inTx = false
	t.conn.pending = nil
	return t.inner.Rollback()
}

type statefulStmt struct {
	conn  *statefulConn
	query string
	inner driver.Stmt
}

var (
	_ driver.Stmt              = (*statefulStmt)(nil)
	_ driver.StmtExecContext   = (*statefulStmt)(nil)
	_ driver.StmtQueryContext  = (*statefulStmt)(nil)
	_ driver.NamedValueChecker = (*statefulStmt)(nil)
)

func (s *statefulStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := s.inner.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return s.conn.CheckNamedValue(nv)
}

func (s *statefulStmt) Close() error  { return s.inner.Close() }
func (s *statefulStmt) NumInput() int { return s.inner.NumInput() }

func (s *statefulStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

func (s *statefulStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *statefulStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	var res driver.Result
	var err error
	if sec, ok := s.inner.(driver.StmtExecContext); ok {
		res, err = sec.ExecContext(ctx, args)
	} else {
		res, err = s.inner.Exec(namedToValues(args))
	}
	_, normSQL, _, _ := sqlClassify(s.query)
	if !isTxControlSQL(normSQL) {
		var affected int64
		if res != nil {
			affected, _ = res.RowsAffected()
		}
		if err == nil {
			s.conn.recordWrite(ctx, s.query, normSQL, formatNamedArgs(args), defaultCommandTag(s.query), affected, nil)
		}
	}
	return res, err
}

func (s *statefulStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	var rows driver.Rows
	var err error
	if sqc, ok := s.inner.(driver.StmtQueryContext); ok {
		rows, err = sqc.QueryContext(ctx, args)
	} else {
		rows, err = s.inner.Query(namedToValues(args))
	}
	if err == nil {
		_, normSQL, isWrite, hasReturning := sqlClassify(s.query)
		if isWrite && hasReturning && !isTxControlSQL(normSQL) {
			s.conn.recordWrite(ctx, s.query, normSQL, formatNamedArgs(args), defaultCommandTag(s.query), 0, nil)
		}
	}
	return rows, err
}

func execInnerQuery(ctx context.Context, inner driver.Conn, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := inner.(driver.QueryerContext); ok {
		rows, err := qc.QueryContext(ctx, query, args)
		if !errors.Is(err, driver.ErrSkip) {
			return rows, err
		}
	}
	stmt, err := inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if sqc, ok := stmt.(driver.StmtQueryContext); ok {
		return sqc.QueryContext(ctx, args)
	}
	return stmt.Query(namedToValues(args))
}

func execInnerExec(ctx context.Context, inner driver.Conn, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := inner.(driver.ExecerContext); ok {
		res, err := ec.ExecContext(ctx, query, args)
		if !errors.Is(err, driver.ErrSkip) {
			return res, err
		}
	}
	stmt, err := inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if sec, ok := stmt.(driver.StmtExecContext); ok {
		return sec.ExecContext(ctx, args)
	}
	return stmt.Exec(namedToValues(args))
}
