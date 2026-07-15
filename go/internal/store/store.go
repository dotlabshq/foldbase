// Package store is the append-only, multi-tenant event log over database/sql.
// No ORM (ADR-006): every value is a bound parameter; identifiers are constants.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// newUUID returns a UUIDv7 string — a 48-bit unix-ms timestamp prefix + random
// tail, so ids are time-ordered (sequential inserts on the `id` index, readable
// creation time). Log ordering stays globalSeq's job; v7 is identity + locality.
// Backed by google/uuid rather than a hand-rolled generator.
func newUUID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString() // fall back to v4 if the clock/entropy read fails
	}
	return id.String()
}

// ValidID reports whether s is a well-formed UUID (any version). Clients MAY
// supply the event id (idempotency / offline / pre-linking causation); the
// server validates it here and generates one when it is absent.
func ValidID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// SchemaSQL is the canonical SQLite/libSQL DDL — identical to the TS
// SQLITE_SCHEMA_SQL. Applied idempotently on boot.
const SchemaSQL = `CREATE TABLE IF NOT EXISTS events (
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

// MigrateSQL is applied after SchemaSQL, best-effort (errors ignored): it
// upgrades a pre-stream_type database in place. Harmless on fresh databases.
const MigrateSQL = `ALTER TABLE events ADD COLUMN stream_type TEXT NOT NULL DEFAULT ''`

// NewEvent is the caller-supplied intent to record a fact.
type NewEvent struct {
	// Optional client-supplied id (UUID). Absent ⇒ the server generates a v7.
	ID            string         `json:"id,omitempty"`
	Type          string         `json:"type"`
	StreamID      string         `json:"streamId"`
	Actor         string         `json:"actor"`
	Payload       map[string]any `json:"payload"`
	CausationID   *string        `json:"causationId,omitempty"`
	CorrelationID *string        `json:"correlationId,omitempty"`
	Metadata      map[string]any `json:"metadata"`
}

// StoredEvent is the immutable fact as it lives in the log.
type StoredEvent struct {
	Tenant        string         `json:"tenant"`
	ID            string         `json:"id"`
	StreamID      string         `json:"streamId"`
	StreamType    string         `json:"streamType"`
	Version       int64          `json:"version"`
	GlobalSeq     int64          `json:"globalSeq"`
	Type          string         `json:"type"`
	Actor         string         `json:"actor"`
	Payload       map[string]any `json:"payload"`
	CausationID   *string        `json:"causationId,omitempty"`
	CorrelationID *string        `json:"correlationId,omitempty"`
	Metadata      map[string]any `json:"metadata"`
	RecordedAt    int64          `json:"recordedAt"`
}

// StreamTypeError is raised when a supplied streamType conflicts with the
// stream's existing type (fixed by its first event).
type StreamTypeError struct{ StreamID, Existing, Supplied string }

func (e *StreamTypeError) Error() string {
	return fmt.Sprintf("stream %q is of type %q; cannot append as %q", e.StreamID, e.Existing, e.Supplied)
}

// ConcurrencyError is raised when the stream moved past expectedVersion.
type ConcurrencyError struct{ Actual int64 }

func (e *ConcurrencyError) Error() string {
	return fmt.Sprintf("concurrency conflict: stream is at version %d", e.Actual)
}

// IsUniqueViolation reports whether err is a UNIQUE constraint failure — the
// DB-level backstop behind optimistic concurrency.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed") || contains(msg, "constraint failed") || contains(msg, "SQLITE_CONSTRAINT")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Store wraps a *sql.DB. NowFunc/IDFunc are injectable for tests.
type Store struct {
	db      *sql.DB
	NowFunc func() int64
	IDFunc  func() string
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) now() int64 {
	if s.NowFunc != nil {
		return s.NowFunc()
	}
	return nowMillis()
}

func (s *Store) id() string {
	if s.IDFunc != nil {
		return s.IDFunc()
	}
	return newUUID()
}

// StreamVersion returns the current version of a stream (0 if none).
func (s *Store) StreamVersion(tenant, streamID string) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(version) FROM events WHERE tenant = ? AND stream_id = ?`,
		tenant, streamID,
	).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// AppendResult mirrors the TS AppendResult.
type AppendResult struct {
	Events  []StoredEvent `json:"events"`
	Version int64         `json:"version"`
}

// Append writes events to the end of a stream with optimistic concurrency.
// The check + insert run in a transaction; the UNIQUE index is the ultimate
// backstop under a race (surfaced as a unique violation → the caller maps 409).
// streamType fixes the stream's kind on first append; a later conflicting
// non-empty value is a StreamTypeError.
func (s *Store) Append(tenant, streamID, streamType string, expectedVersion int64, events []NewEvent) (*AppendResult, error) {
	if len(events) == 0 {
		return &AppendResult{Events: []StoredEvent{}, Version: expectedVersion}, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Version + existing stream type in one aggregate query (all events of a
	// stream share one stream_type, so MAX() reads it for free).
	var cur int64
	var nv sql.NullInt64
	var nt sql.NullString
	if err := tx.QueryRow(`SELECT MAX(version), MAX(stream_type) FROM events WHERE tenant = ? AND stream_id = ?`, tenant, streamID).Scan(&nv, &nt); err != nil {
		return nil, err
	}
	if nv.Valid {
		cur = nv.Int64
	}
	if cur != expectedVersion {
		return nil, &ConcurrencyError{Actual: cur}
	}
	existing := ""
	if nt.Valid {
		existing = nt.String
	}
	if cur > 0 && streamType != "" && existing != streamType {
		return nil, &StreamTypeError{StreamID: streamID, Existing: existing, Supplied: streamType}
	}
	if cur > 0 {
		streamType = existing
	}

	now := s.now()
	stored := make([]StoredEvent, 0, len(events))
	for i, e := range events {
		version := cur + int64(i) + 1
		// Client-supplied id wins (idempotency / offline); else generate.
		id := e.ID
		if id == "" {
			id = s.id()
		}
		payload := e.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		metadata := e.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		pj, _ := json.Marshal(payload)
		mj, _ := json.Marshal(metadata)

		res, err := tx.Exec(
			`INSERT INTO events
			 (id, tenant, stream_id, stream_type, version, type, actor, payload, causation_id, correlation_id, metadata, recorded_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, tenant, streamID, streamType, version, e.Type, e.Actor, string(pj),
			nullStr(e.CausationID), nullStr(e.CorrelationID), string(mj), now,
		)
		if err != nil {
			return nil, err
		}
		gs, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		stored = append(stored, StoredEvent{
			Tenant: tenant, ID: id, StreamID: streamID, StreamType: streamType, Version: version, GlobalSeq: gs,
			Type: e.Type, Actor: e.Actor, Payload: payload,
			CausationID: e.CausationID, CorrelationID: e.CorrelationID,
			Metadata: metadata, RecordedAt: now,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &AppendResult{Events: stored, Version: cur + int64(len(events))}, nil
}

const selectCols = `global_seq, id, tenant, stream_id, stream_type, version, type, actor, payload, causation_id, correlation_id, metadata, recorded_at`

// limitClause appends a LIMIT when limit > 0 (0 = unbounded, internal replay).
func limitClause(sqlStr string, limit int64, args []any) (string, []any) {
	if limit > 0 {
		return sqlStr + ` LIMIT ?`, append(args, limit)
	}
	return sqlStr, args
}

// ReadStream reads one stream's history in version order (limit 0 = all).
func (s *Store) ReadStream(tenant, streamID string, fromVersion, limit int64) ([]StoredEvent, error) {
	q, args := limitClause(
		`SELECT `+selectCols+` FROM events WHERE tenant = ? AND stream_id = ? AND version > ? ORDER BY version ASC`,
		limit, []any{tenant, streamID, fromVersion},
	)
	rows, err := s.db.Query(q, args...)
	return scanEvents(rows, err)
}

// ReadByCorrelation reads events with a correlationId, in global order.
func (s *Store) ReadByCorrelation(tenant, correlationID string, limit int64) ([]StoredEvent, error) {
	q, args := limitClause(
		`SELECT `+selectCols+` FROM events WHERE tenant = ? AND correlation_id = ? ORDER BY global_seq ASC`,
		limit, []any{tenant, correlationID},
	)
	rows, err := s.db.Query(q, args...)
	return scanEvents(rows, err)
}

// ReadAll reads the tenant log in global order. streamType "" = every stream;
// otherwise a category read. limit 0 = unbounded (internal replay).
func (s *Store) ReadAll(tenant string, fromGlobalSeq int64, streamType string, limit int64) ([]StoredEvent, error) {
	sqlStr := `SELECT ` + selectCols + ` FROM events WHERE tenant = ? AND global_seq > ?`
	args := []any{tenant, fromGlobalSeq}
	if streamType != "" {
		sqlStr += ` AND stream_type = ?`
		args = append(args, streamType)
	}
	sqlStr += ` ORDER BY global_seq ASC`
	q, args := limitClause(sqlStr, limit, args)
	rows, err := s.db.Query(q, args...)
	return scanEvents(rows, err)
}

func scanEvents(rows *sql.Rows, err error) ([]StoredEvent, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StoredEvent{}
	for rows.Next() {
		var (
			e             StoredEvent
			payload, meta string
			caus, corr    sql.NullString
		)
		if err := rows.Scan(&e.GlobalSeq, &e.ID, &e.Tenant, &e.StreamID, &e.StreamType, &e.Version, &e.Type, &e.Actor,
			&payload, &caus, &corr, &meta, &e.RecordedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(meta), &e.Metadata); err != nil {
			return nil, err
		}
		if caus.Valid {
			e.CausationID = &caus.String
		}
		if corr.Valid {
			e.CorrelationID = &corr.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
