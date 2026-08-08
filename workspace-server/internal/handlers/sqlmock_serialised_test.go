package handlers

// A serialising driver shim for go-sqlmock.
//
// WHY THIS EXISTS
//
// go-sqlmock is not goroutine-safe, and the unsafety is structural rather than
// incidental. mockDriver.Open resolves a DSN against a shared map and returns
// the SAME *sqlmock instance to every connection database/sql opens, so a pool
// of N driverConns aliases one mock. database/sql does hold a per-driverConn
// lock across each driver call — that is what makes a real driver safe behind a
// concurrent *sql.DB — but with every driverConn pointing at one mock, that
// lock serialises nothing.
//
// Two unsynchronised paths then collide on the same expectation list:
//
//   - (*queryBasedExpectation).argsMatches invokes a user-supplied
//     Argument.Match while matching a statement (expectations_go18.go:47), and
//   - (*sqlmock).exec formats a mismatch through fmt.Errorf, which reflects
//     over every argument the expectation holds via (*ExpectedQuery).String
//     (expectations.go:187).
//
// Any handler that dispatches goroutines while its own request goroutine keeps
// issuing statements — Resume's cascade fans out a provisioner per claimed
// descendant and carries on claiming the rest — puts one goroutine in each path
// at once. The race detector reports it against whichever object the
// expectation holds, which for these fixtures is the test's own probe struct:
// the probe locks its mutex in Match while the formatter reads the same fields
// reflectively, and no lock the probe could take would help, because the
// reflective read never takes one.
//
// WHAT THIS CHANGES
//
// Nothing observable. Every statement still runs, in the order the production
// code issues it, against the same expectations. The concurrency under test is
// untouched: the provisioner goroutines still run concurrently with the
// handler and still race each other for expectation slots — they simply stop
// corrupting the mock's internals while doing so. The mutex is held only for
// the duration of a single inner driver call, and go-sqlmock materialises its
// rows before returning, so no lock is ever held across row iteration and this
// cannot deadlock a caller that queries while scanning.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/DATA-DOG/go-sqlmock"
)

var serialisedMockCounter atomic.Int64

// newSerialisedSqlmock builds a sqlmock whose every driver call is serialised.
//
// It returns the *sql.DB the code under test should use, the raw *sql.DB that
// owns the mock's pool entry (it must outlive the serialised handle and be
// closed after it), and the Sqlmock used to set expectations.
func newSerialisedSqlmock() (serialised *sql.DB, raw *sql.DB, mock sqlmock.Sqlmock, err error) {
	// NewWithDSN rather than New: the serialising connector has to reopen the
	// same DSN, and New keeps its generated DSN to itself.
	dsn := fmt.Sprintf("sqlmock_serialised_%d", serialisedMockCounter.Add(1))
	raw, mock, err = sqlmock.NewWithDSN(dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	serialised = sql.OpenDB(&serialisingConnector{
		inner: raw.Driver(),
		dsn:   dsn,
		mu:    new(sync.Mutex),
	})
	return serialised, raw, mock, nil
}

// ─── connector ───────────────────────────────────────────────────────────────

type serialisingConnector struct {
	inner driver.Driver
	dsn   string
	// mu is shared by every connection this connector hands out, because every
	// one of them resolves to the same *sqlmock.
	mu *sync.Mutex
}

func (c *serialisingConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &serialisingConn{inner: inner, mu: c.mu}, nil
}

func (c *serialisingConnector) Driver() driver.Driver { return c.inner }

// ─── connection ──────────────────────────────────────────────────────────────

type serialisingConn struct {
	inner driver.Conn
	mu    *sync.Mutex
}

var (
	_ driver.Conn               = (*serialisingConn)(nil)
	_ driver.ConnPrepareContext = (*serialisingConn)(nil)
	_ driver.ConnBeginTx        = (*serialisingConn)(nil)
	_ driver.ExecerContext      = (*serialisingConn)(nil)
	_ driver.QueryerContext     = (*serialisingConn)(nil)
	_ driver.Pinger             = (*serialisingConn)(nil)
)

func (c *serialisingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *serialisingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *serialisingConn) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.(driver.Pinger).Ping(ctx)
}

func (c *serialisingConn) Prepare(query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &serialisingStmt{inner: stmt, conn: c, query: query}, nil
}

func (c *serialisingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stmt, err := c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &serialisingStmt{inner: stmt, conn: c, query: query}, nil
}

// Begin routes through BeginTx rather than the inner Conn.Begin, which is
// deprecated (SA1019). driver.Conn still requires the method to exist.
func (c *serialisingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *serialisingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx, err := c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &serialisingTx{inner: tx, mu: c.mu}, nil
}

func (c *serialisingConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.Close()
}

// ─── statement ───────────────────────────────────────────────────────────────

// serialisingStmt keeps the inner statement only for Close and NumInput.
// Exec and Query are routed back through the connection's context methods —
// which is exactly what go-sqlmock's own statement does (it forwards to
// conn.ExecContext with the prepared query) — so the locking lives in one place
// and no deprecated Stmt method is ever called (SA1019).
type serialisingStmt struct {
	inner driver.Stmt
	conn  *serialisingConn
	query string
}

var (
	_ driver.Stmt             = (*serialisingStmt)(nil)
	_ driver.StmtExecContext  = (*serialisingStmt)(nil)
	_ driver.StmtQueryContext = (*serialisingStmt)(nil)
)

// NumInput takes no lock: go-sqlmock's statement returns a constant.
func (s *serialisingStmt) NumInput() int { return s.inner.NumInput() }

func (s *serialisingStmt) Close() error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	return s.inner.Close()
}

func (s *serialisingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *serialisingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

// Exec and Query satisfy driver.Stmt, which still requires them.
func (s *serialisingStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s *serialisingStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func namedValues(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// ─── transaction ─────────────────────────────────────────────────────────────

// go-sqlmock returns the connection itself as the Tx, so Commit and Rollback
// land on the same shared instance and need the same mutex.
type serialisingTx struct {
	inner driver.Tx
	mu    *sync.Mutex
}

var _ driver.Tx = (*serialisingTx)(nil)

func (t *serialisingTx) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inner.Commit()
}

func (t *serialisingTx) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inner.Rollback()
}
