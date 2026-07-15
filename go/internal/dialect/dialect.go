// Package dialect isolates the few SQL differences between SQLite/libsql and
// PostgreSQL so the engine can stay dialect-blind (ADR-006/010). The engine
// emits `?`-placeholder SQL and lowercase column types; the dialect rewrites
// placeholders, maps column types, and owns the handful of DDL/introspection
// statements that actually differ.
package dialect

import (
	"database/sql"
	"strconv"
	"strings"
)

type Kind int

const (
	SQLite Kind = iota
	Postgres
)

type Dialect struct{ Kind Kind }

func (d Dialect) String() string {
	if d.Kind == Postgres {
		return "postgres"
	}
	return "sqlite"
}

// Rewrite converts `?` placeholders to `$1, $2, …` for Postgres; SQLite keeps `?`.
// Values are always bound parameters, so this is a purely positional rewrite —
// it never touches identifiers or literals (there are none by construction).
func (d Dialect) Rewrite(q string) string {
	if d.Kind != Postgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 1
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			n++
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

// ColumnType maps a read-model column type to the dialect's SQL type. On
// Postgres, `integer` MUST be BIGINT: read models store epoch-ms timestamps and
// counters that overflow a 32-bit INTEGER.
func (d Dialect) ColumnType(t string) string {
	if d.Kind == Postgres {
		switch t {
		case "integer":
			return "BIGINT"
		case "real":
			return "DOUBLE PRECISION"
		default:
			return "TEXT"
		}
	}
	switch t {
	case "integer":
		return "INTEGER"
	case "real":
		return "REAL"
	default:
		return "TEXT"
	}
}

// EventsSchema returns the log DDL (multiple statements). The only real
// difference is the autoincrement primary key and 64-bit integer columns.
func (d Dialect) EventsSchema() string {
	if d.Kind == Postgres {
		return `CREATE TABLE IF NOT EXISTS events (
  global_seq     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id             TEXT   NOT NULL UNIQUE,
  tenant         TEXT   NOT NULL,
  stream_id      TEXT   NOT NULL,
  stream_type    TEXT   NOT NULL DEFAULT '',
  version        BIGINT NOT NULL,
  type           TEXT   NOT NULL,
  actor          TEXT   NOT NULL,
  payload        TEXT   NOT NULL,
  causation_id   TEXT,
  correlation_id TEXT,
  metadata       TEXT   NOT NULL,
  recorded_at    BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS events_tenant_stream_version_uq ON events (tenant, stream_id, version);`
	}
	return `CREATE TABLE IF NOT EXISTS events (
  global_seq     INTEGER PRIMARY KEY AUTOINCREMENT,
  id             TEXT    NOT NULL UNIQUE,
  tenant         TEXT    NOT NULL,
  stream_id      TEXT    NOT NULL,
  stream_type    TEXT    NOT NULL DEFAULT '',
  version        INTEGER NOT NULL,
  type           TEXT    NOT NULL,
  actor          TEXT    NOT NULL,
  payload        TEXT    NOT NULL,
  causation_id   TEXT,
  correlation_id TEXT,
  metadata       TEXT    NOT NULL,
  recorded_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS events_tenant_stream_version_uq
  ON events (tenant, stream_id, version);`
}

// RegistryDDL returns the definition tables (_projections/_policies/_rpc).
// updated_at is BIGINT-equivalent so epoch-ms values don't overflow on Postgres.
func (d Dialect) RegistryDDL() []string {
	ts := d.ColumnType("integer")
	return []string{
		`CREATE TABLE IF NOT EXISTS _projections (name TEXT PRIMARY KEY, def TEXT NOT NULL, updated_at ` + ts + ` NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _policies (name TEXT NOT NULL, role TEXT NOT NULL, action TEXT NOT NULL DEFAULT 'select', def TEXT NOT NULL, updated_at ` + ts + ` NOT NULL, PRIMARY KEY (name, role, action))`,
		`CREATE TABLE IF NOT EXISTS _rpc (name TEXT PRIMARY KEY, def TEXT NOT NULL, updated_at ` + ts + ` NOT NULL)`,
	}
}

// ExistingColumns lists a table's columns — PRAGMA on SQLite, information_schema
// on Postgres. Used by the registry to add missing columns additively.
func (d Dialect) ExistingColumns(exec interface {
	Query(string, ...any) (*sql.Rows, error)
}, table string) (map[string]bool, error) {
	cols := map[string]bool{}
	if d.Kind == Postgres {
		rows, err := exec.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = ?`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			cols[name] = true
		}
		return cols, rows.Err()
	}
	// SQLite: PRAGMA table_info returns (cid, name, type, notnull, dflt, pk).
	rows, err := exec.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// IsUniqueViolation reports whether err is a UNIQUE constraint failure — the
// backstop behind optimistic concurrency. SQLite reports it in the message,
// Postgres via SQLSTATE 23505.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT") ||
		strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key value")
}
