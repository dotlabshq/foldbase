package readmodel

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/dialect"
)

// setupBool registers a projection with a declared boolean column and folds
// three rows: done=true, done=false, and one where the rule never sets it.
func setupBool(t *testing.T) (*sql.DB, *Registry) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	reg := NewRegistry(db, dialect.Dialect{Kind: dialect.SQLite})
	if err := reg.Init(); err != nil {
		t.Fatal(err)
	}
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "tasks",
		Columns: map[string]string{"owner": "text", "done": "boolean"},
		On: map[string]OpRule{
			"TaskAdded": {Op: "upsert", Set: map[string]any{"owner": "$.owner", "done": "$.done"}},
			"TaskFiled": {Op: "upsert", Set: map[string]any{"owner": "$.owner"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SavePolicy(&PolicyDef{Name: "tasks", Role: "*", Using: "owner = :auth_uid"}); err != nil {
		t.Fatal(err)
	}
	apply := func(e EventLike) {
		if err := ApplyEvent(db, reg, e); err != nil {
			t.Fatal(err)
		}
	}
	apply(EventLike{Type: "TaskAdded", StreamID: "t1", Tenant: "acme", Payload: map[string]any{"owner": "u1", "done": true}})
	apply(EventLike{Type: "TaskAdded", StreamID: "t2", Tenant: "acme", Payload: map[string]any{"owner": "u1", "done": false}})
	apply(EventLike{Type: "TaskFiled", StreamID: "t3", Tenant: "acme", Payload: map[string]any{"owner": "u1"}})
	return db, reg
}

func rowByID(t *testing.T, rows []map[string]any, id string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	t.Fatalf("no row with id %q in %+v", id, rows)
	return nil
}

// A boolean folded in must come back out as a JSON boolean, not as 0/1.
func TestBooleanColumnReadsBackAsBoolean(t *testing.T) {
	db, reg := setupBool(t)
	r, err := ExecQuery(db, reg, "tasks", map[string]any{"sort": []any{"id"}}, AuthCtx{Tenant: "acme", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowByID(t, r.Rows, "t1")["done"]; got != true {
		t.Fatalf("done for t1: got %#v, want true", got)
	}
	if got := rowByID(t, r.Rows, "t2")["done"]; got != false {
		t.Fatalf("done for t2: got %#v, want false", got)
	}
}

// A boolean column no rule ever set stays null — absence is not false.
func TestBooleanColumnNullStaysNull(t *testing.T) {
	db, reg := setupBool(t)
	r, err := ExecQuery(db, reg, "tasks", map[string]any{"sort": []any{"id"}}, AuthCtx{Tenant: "acme", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowByID(t, r.Rows, "t3")["done"]; got != nil {
		t.Fatalf("done for t3: got %#v, want nil", got)
	}
}

// eq already normalized; in did not. Both must filter a boolean column.
func TestBooleanFilterEqAndIn(t *testing.T) {
	db, reg := setupBool(t)
	ctx := AuthCtx{Tenant: "acme", UID: "u1"}

	eq, err := ExecQuery(db, reg, "tasks", map[string]any{
		"where": map[string]any{"done": map[string]any{"eq": true}},
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(eq.Rows) != 1 || eq.Rows[0]["id"] != "t1" {
		t.Fatalf("eq true: got %+v, want just t1", eq.Rows)
	}

	in, err := ExecQuery(db, reg, "tasks", map[string]any{
		"where": map[string]any{"done": map[string]any{"in": []any{true}}},
	}, ctx)
	if err != nil {
		t.Fatalf("in [true]: %v", err)
	}
	if len(in.Rows) != 1 || in.Rows[0]["id"] != "t1" {
		t.Fatalf("in [true]: got %+v, want just t1", in.Rows)
	}
}

// boolean is a declarable column type, stored exactly as integer is — so
// switching a column from integer to boolean needs no DDL and no rebuild.
func TestBooleanIsStoredAsInteger(t *testing.T) {
	if got := (dialect.Dialect{Kind: dialect.SQLite}).ColumnType("boolean"); got != "INTEGER" {
		t.Fatalf("sqlite boolean column type: got %q, want INTEGER", got)
	}
	if got := (dialect.Dialect{Kind: dialect.Postgres}).ColumnType("boolean"); got != "BIGINT" {
		t.Fatalf("postgres boolean column type: got %q, want BIGINT", got)
	}
}

// Every where-clause operator must hand SQL the same 0/1 the projector wrote.
// A raw Go bool reaching a bind parameter works by accident on SQLite and
// fails against a BIGINT column on Postgres, so assert on the args directly.
func TestWhereBindsBooleansAsIntegers(t *testing.T) {
	cols := map[string]bool{"done": true}
	cases := map[string]any{
		"eq": map[string]any{"done": map[string]any{"eq": true}},
		"ne": map[string]any{"done": map[string]any{"ne": false}},
		"in": map[string]any{"done": map[string]any{"in": []any{true, false}}},
	}
	for name, node := range cases {
		_, args, err := compileWhere(node, cols)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, a := range args {
			if _, isBool := a.(bool); isBool {
				t.Fatalf("%s: arg %d is a raw bool (%#v); want 0/1", name, i, a)
			}
		}
	}
}
