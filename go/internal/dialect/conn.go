package dialect

import "database/sql"

// Conn wraps *sql.DB and rewrites `?` placeholders per dialect on every call, so
// the engine (which always emits `?`) stays dialect-blind. Satisfies the
// readmodel SQLDB interface (Exec/Query/QueryRow).
type Conn struct {
	DB *sql.DB
	D  Dialect
}

func New(db *sql.DB, d Dialect) *Conn { return &Conn{DB: db, D: d} }

func (c *Conn) Exec(q string, args ...any) (sql.Result, error) { return c.DB.Exec(c.D.Rewrite(q), args...) }
func (c *Conn) Query(q string, args ...any) (*sql.Rows, error) { return c.DB.Query(c.D.Rewrite(q), args...) }
func (c *Conn) QueryRow(q string, args ...any) *sql.Row        { return c.DB.QueryRow(c.D.Rewrite(q), args...) }
func (c *Conn) Close() error                                   { return c.DB.Close() }

// Begin returns a rewriting transaction (used by the store's append path).
func (c *Conn) Begin() (*Tx, error) {
	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, d: c.D}, nil
}

// Tx is a rewriting transaction wrapper.
type Tx struct {
	tx *sql.Tx
	d  Dialect
}

func (t *Tx) Exec(q string, args ...any) (sql.Result, error) { return t.tx.Exec(t.d.Rewrite(q), args...) }
func (t *Tx) Query(q string, args ...any) (*sql.Rows, error) { return t.tx.Query(t.d.Rewrite(q), args...) }
func (t *Tx) QueryRow(q string, args ...any) *sql.Row        { return t.tx.QueryRow(t.d.Rewrite(q), args...) }
func (t *Tx) Commit() error                                  { return t.tx.Commit() }
func (t *Tx) Rollback() error                                { return t.tx.Rollback() }
