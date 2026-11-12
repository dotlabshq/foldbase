package readmodel

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/dialect"
)

// Reading history written before a rename: a snake_case path that is absent in
// a payload is read from its camelCase spelling.

func TestCamelCase(t *testing.T) {
	for in, want := range map[string]string{
		"external_id": "externalId", "owner_id": "ownerId", "a_b_c": "aBC", "plain": "plain",
		"": "", "x_": "x", "_x": "X", "at": "at", "tax_id": "taxId", "org_id2": "orgId2",
	} {
		if got := camelCase(in); got != want {
			t.Errorf("camelCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPayloadPathFallsBackPerSegment(t *testing.T) {
	old := map[string]any{"ownerId": "u1", "line": map[string]any{"unitPrice": 5.0}, "lineItems": map[string]any{"unitPrice": 7.0}}
	for path, want := range map[string]any{
		"$.owner_id": "u1", "$.line.unit_price": 5.0, "$.line_items.unit_price": 7.0, "$.ownerId": "u1",
	} {
		v, ok := payloadPath(old, path)
		if !ok || v != want {
			t.Errorf("%s: got %v/%v, want %v", path, v, ok, want)
		}
	}
	if _, ok := payloadPath(old, "$.owner_name"); ok {
		t.Error("absent in both spellings must stay absent")
	}
	if _, ok := payloadPath(old, "$.line.unit_cost"); ok {
		t.Error("absent nested in both spellings must stay absent")
	}
	if _, ok := payloadPath(map[string]any{"owner_id": "x"}, "$.ownerId"); ok {
		t.Error("the fallback goes snake to camel only, never the other way")
	}
}

func TestSnakeKeyWinsOverCamel(t *testing.T) {
	v, ok := payloadPath(map[string]any{"owner_id": "new", "ownerId": "old"}, "$.owner_id")
	if !ok || v != "new" {
		t.Fatalf("got %v/%v, want the snake_case key", v, ok)
	}
	// a snake_case key that exists with null still wins: null is a fact, not absence
	v, ok = payloadPath(map[string]any{"owner_id": nil, "ownerId": "old"}, "$.owner_id")
	if !ok || v != nil {
		t.Fatalf("got %v/%v, want an explicit null", v, ok)
	}
}

func snakeDB(t *testing.T) (*sql.DB, *Registry) {
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
		Name:    "things",
		Columns: map[string]string{"owner_id": "text", "unit_price": "real"},
		On: map[string]OpRule{
			"ThingMade": {Op: "upsert", Set: map[string]any{"owner_id": "$.owner_id", "unit_price": "$.line.unit_price"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// rows identified by a payload field: every rule of a projection is keyed, or none
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "moves",
		Columns: map[string]string{"qty": "integer", "n": "integer"},
		On: map[string]OpRule{
			"ThingMoved": {Op: "upsert", Key: "$.thing_id", Inc: map[string]any{"qty": "$.move_qty", "n": 1}},
			"ThingGone":  {Op: "delete", Key: "$.thing_id"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return db, reg
}

func thing(t *testing.T, db *sql.DB, id string) (owner sql.NullString, price sql.NullFloat64, qty sql.NullInt64, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT owner_id, unit_price FROM read_things WHERE tenant = ? AND id = ?`, "acme", id).Scan(&owner, &price)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	ok = true
	return
}

func qtyOf(t *testing.T, db *sql.DB, id string) (int64, bool) {
	t.Helper()
	var q int64
	err := db.QueryRow(`SELECT qty FROM read_moves WHERE tenant = ? AND id = ?`, "acme", id).Scan(&q)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return q, true
}

func TestSetReadsOldAndNewEventsIntoTheSameRow(t *testing.T) {
	db, reg := snakeDB(t)
	old := EventLike{Type: "ThingMade", StreamID: "old", Tenant: "acme", Payload: map[string]any{"ownerId": "u1", "line": map[string]any{"unitPrice": 5.0}}}
	nw := EventLike{Type: "ThingMade", StreamID: "new", Tenant: "acme", Payload: map[string]any{"owner_id": "u1", "line": map[string]any{"unit_price": 5.0}}}
	for _, e := range []EventLike{old, nw} {
		if err := ApplyEvent(db, reg, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"old", "new"} {
		o, p, _, ok := thing(t, db, id)
		if !ok || o.String != "u1" || p.Float64 != 5 {
			t.Errorf("%s: got %v %v %v", id, o, p, ok)
		}
	}
}

func TestSetWithBothSpellingsTakesSnake(t *testing.T) {
	db, reg := snakeDB(t)
	if err := ApplyEvent(db, reg, EventLike{Type: "ThingMade", StreamID: "b", Tenant: "acme", Payload: map[string]any{"owner_id": "new", "ownerId": "old"}}); err != nil {
		t.Fatal(err)
	}
	if o, _, _, _ := thing(t, db, "b"); o.String != "new" {
		t.Fatalf("got %q, want the snake_case value", o.String)
	}
}

func TestSetNullAndAbsentStayNull(t *testing.T) {
	db, reg := snakeDB(t)
	for id, payload := range map[string]map[string]any{
		"null":    {"owner_id": nil, "ownerId": "old"},
		"absent":  {"something": 1.0},
		"oldnull": {"ownerId": nil},
	} {
		if err := ApplyEvent(db, reg, EventLike{Type: "ThingMade", StreamID: id, Tenant: "acme", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if o, _, _, ok := thing(t, db, id); !ok || o.Valid {
			t.Errorf("%s: owner should be NULL, got %v", id, o)
		}
	}
}

func TestIncAndKeyReadOldAndNewSpellings(t *testing.T) {
	db, reg := snakeDB(t)
	events := []EventLike{
		{Type: "ThingMoved", StreamID: "s1", Tenant: "acme", Payload: map[string]any{"thingId": "t1", "moveQty": 4.0}},   // before the rename
		{Type: "ThingMoved", StreamID: "s2", Tenant: "acme", Payload: map[string]any{"thing_id": "t1", "move_qty": 6.0}}, // after
		{Type: "ThingMoved", StreamID: "s3", Tenant: "acme", Payload: map[string]any{"thing_id": "t2", "moveQty": -1.0}}, // mixed
	}
	for _, e := range events {
		if err := ApplyEvent(db, reg, e); err != nil {
			t.Fatal(err)
		}
	}
	if q, ok := qtyOf(t, db, "t1"); !ok || q != 10 {
		t.Fatalf("t1 qty: got %v/%v, want 10", q, ok)
	}
	if q, ok := qtyOf(t, db, "t2"); !ok || q != -1 {
		t.Fatalf("t2 qty: got %v/%v, want -1", q, ok)
	}
	// a delete keyed by the old spelling removes the row an upsert keyed by the new one made
	if err := ApplyEvent(db, reg, EventLike{Type: "ThingGone", StreamID: "g", Tenant: "acme", Payload: map[string]any{"thingId": "t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := qtyOf(t, db, "t1"); ok {
		t.Fatal("t1 should be gone")
	}
}

func TestIncAbsentInBothSpellingsIsStillAnError(t *testing.T) {
	db, reg := snakeDB(t)
	err := ApplyEvent(db, reg, EventLike{Type: "ThingMoved", StreamID: "s", Tenant: "acme", Payload: map[string]any{"thing_id": "t9", "qty": 3.0}})
	if err == nil {
		t.Fatal("an inc path absent in both spellings must fail the fold")
	}
	if _, ok := err.(*FoldError); !ok {
		t.Fatalf("want a FoldError, got %T", err)
	}
}

func TestKeyAbsentInBothSpellingsIsStillAnError(t *testing.T) {
	db, reg := snakeDB(t)
	if err := ApplyEvent(db, reg, EventLike{Type: "ThingMoved", StreamID: "s", Tenant: "acme", Payload: map[string]any{"move_qty": 3.0}}); err == nil {
		t.Fatal("a key that resolves under neither spelling must fail the fold")
	}
}
