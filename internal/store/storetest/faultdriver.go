// Package storetest checks store backends from their tests: a database
// driver wrapper that makes every transaction commit fail, and scenarios that
// drive each store write to its commit.
package storetest

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync/atomic"
)

// ErrCommitFault is the error a commit returns while the fault is on.
var ErrCommitFault = errors.New("injected commit failure")

// FaultDriver wraps a database/sql driver. While Fail is on, every
// transaction commit rolls the transaction back and returns ErrCommitFault.
// Statements outside a transaction run as usual.
type FaultDriver struct {
	inner   driver.Driver
	failing atomic.Bool
	failed  atomic.Int64
}

// NewFaultDriver wraps inner. Register it with sql.Register under a name of
// the test's own.
func NewFaultDriver(inner driver.Driver) *FaultDriver {
	return &FaultDriver{inner: inner}
}

// Fail turns the commit fault on or off.
func (d *FaultDriver) Fail(on bool) { d.failing.Store(on) }

// Failed returns how many commits the fault has failed so far.
func (d *FaultDriver) Failed() int64 { return d.failed.Load() }

// Open opens a connection of the wrapped driver.
func (d *FaultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, d: d}, nil
}

type faultConn struct {
	driver.Conn
	d *FaultDriver
}

func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	var err error
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = b.BeginTx(ctx, opts)
	} else {
		tx, err = c.Conn.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
	}
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, d: c.d}, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultConn) CheckNamedValue(v *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(v)
	}
	return driver.ErrSkip
}

func (c *faultConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *faultConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *faultConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

type faultTx struct {
	driver.Tx
	d *FaultDriver
}

func (t *faultTx) Commit() error {
	if t.d.failing.Load() {
		t.d.failed.Add(1)
		if err := t.Tx.Rollback(); err != nil {
			return errors.Join(ErrCommitFault, err)
		}
		return ErrCommitFault
	}
	return t.Tx.Commit()
}
