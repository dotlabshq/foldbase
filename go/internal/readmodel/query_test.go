package readmodel

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setup(t *testing.T) (*sql.DB, *Registry) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	reg := NewRegistry(db)
	if err := reg.Init(); err != nil {
		t.Fatal(err)
	}
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "notes",
		Columns: map[string]string{"owner": "text", "body": "text"},
		On:      map[string]OpRule{"NoteAdded": {Op: "upsert", Set: map[string]any{"owner": "$.owner", "body": "$.body"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SavePolicy(&PolicyDef{Name: "notes", Role: "*", Using: "owner = :auth_uid"}); err != nil {
		t.Fatal(err)
	}
	_ = ApplyEvent(db, reg, EventLike{Type: "NoteAdded", StreamID: "n1", Tenant: "acme", Payload: map[string]any{"owner": "u1", "body": "x"}})
	_ = ApplyEvent(db, reg, EventLike{Type: "NoteAdded", StreamID: "n2", Tenant: "acme", Payload: map[string]any{"owner": "u2", "body": "y"}})
	return db, reg
}

func TestPolicyScopesRows(t *testing.T) {
	db, reg := setup(t)
	r, err := ExecQuery(db, reg, "notes", map[string]any{}, AuthCtx{Tenant: "acme", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0]["owner"] != "u1" {
		t.Fatalf("policy scoping wrong: %+v", r.Rows)
	}
}

func TestDenyByDefaultAndUnderscoreUnreachable(t *testing.T) {
	db, reg := setup(t)
	// no uid → policy unsatisfiable → forbidden
	if _, err := ExecQuery(db, reg, "notes", map[string]any{}, AuthCtx{Tenant: "acme"}); err == nil {
		t.Fatal("expected forbidden without uid")
	} else if _, ok := err.(*ForbiddenError); !ok {
		t.Fatalf("expected ForbiddenError, got %T", err)
	}
	// underscore tables structurally unreachable
	if _, err := ExecQuery(db, reg, "_policies", map[string]any{}, AuthCtx{Tenant: "acme", UID: "u1"}); err == nil {
		t.Fatal("expected not-found for _policies")
	} else if _, ok := err.(*NotFoundError); !ok {
		t.Fatalf("expected NotFoundError, got %T", err)
	}
}

func TestHostileInputRejected(t *testing.T) {
	db, reg := setup(t)
	// SQL injection attempt in select → validation error, never reaches SQL
	_, err := ExecQuery(db, reg, "notes",
		map[string]any{"select": []any{"id; DROP TABLE read_notes"}},
		AuthCtx{Tenant: "acme", UID: "u1"})
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	// unknown column in where → validation error
	_, err = ExecQuery(db, reg, "notes",
		map[string]any{"where": map[string]any{"evil_col": map[string]any{"eq": "x"}}},
		AuthCtx{Tenant: "acme", UID: "u1"})
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("expected ValidationError for unknown column, got %v", err)
	}
	// the table survived
	r, _ := ExecQuery(db, reg, "notes", map[string]any{}, AuthCtx{Tenant: "acme", UID: "u1"})
	if len(r.Rows) != 1 {
		t.Fatal("table damaged by hostile input")
	}
}

func TestLimitDefaultsAndClamp(t *testing.T) {
	db, reg := setup(t)
	r, err := ExecQuery(db, reg, "notes", map[string]any{}, AuthCtx{Tenant: "acme", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Limit != 1000 {
		t.Fatalf("default limit should be 1000, got %d", r.Limit)
	}
	r2, _ := ExecQuery(db, reg, "notes", map[string]any{"limit": float64(50000)}, AuthCtx{Tenant: "acme", UID: "u1"})
	if r2.Limit != 10000 {
		t.Fatalf("limit should clamp to 10000, got %d", r2.Limit)
	}
}
