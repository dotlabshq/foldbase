package readmodel

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/dialect"
)

// setupKeyed registers a stock projection whose rows are identified by a field
// in the payload rather than by the stream the event landed on.
func setupKeyed(t *testing.T) (*sql.DB, *Registry) {
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
		Name:    "stock",
		Columns: map[string]string{"on_hand": "integer", "warehouse": "text"},
		On: map[string]OpRule{
			"StockMoved":      {Op: "upsert", Key: "$.sku", Set: map[string]any{"warehouse": "$.warehouse"}, Inc: map[string]any{"on_hand": 1}},
			"SkuDiscontinued": {Op: "delete", Key: "$.sku"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return db, reg
}

func stockRow(t *testing.T, db *sql.DB, id string) (int64, bool) {
	t.Helper()
	var n int64
	err := db.QueryRow(`SELECT on_hand FROM read_stock WHERE tenant = ? AND id = ?`, "acme", id).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return n, true
}

func move(stream string, payload map[string]any) EventLike {
	return EventLike{Type: "StockMoved", StreamID: stream, Tenant: "acme", Payload: payload}
}

// The point of the change: a total that spans streams. Two different streams
// carrying the same sku land on one row.
func TestKeyGroupsRowsAcrossStreams(t *testing.T) {
	db, reg := setupKeyed(t)
	for _, stream := range []string{"order-1", "shipment-9"} {
		if err := ApplyEvent(db, reg, move(stream, map[string]any{"sku": "A1", "warehouse": "w1"})); err != nil {
			t.Fatalf("fold: %v", err)
		}
	}
	n, ok := stockRow(t, db, "A1")
	if !ok {
		t.Fatal("no row keyed A1")
	}
	if n != 2 {
		t.Fatalf("on_hand: got %d, want 2", n)
	}
	if _, ok := stockRow(t, db, "order-1"); ok {
		t.Fatal("a keyed rule must not also write a row keyed by the stream")
	}
}

// What makes `key` more than cosmetic: one stream, several rows.
func TestOneStreamWritesSeveralKeys(t *testing.T) {
	db, reg := setupKeyed(t)
	for _, sku := range []string{"A1", "B2"} {
		if err := ApplyEvent(db, reg, move("order-1", map[string]any{"sku": sku, "warehouse": "w1"})); err != nil {
			t.Fatalf("fold: %v", err)
		}
	}
	for _, sku := range []string{"A1", "B2"} {
		if _, ok := stockRow(t, db, sku); !ok {
			t.Fatalf("no row keyed %s", sku)
		}
	}
}

// delete follows the same key, or it has nothing to point at.
func TestDeleteUsesTheSameKey(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := ApplyEvent(db, reg, move("order-1", map[string]any{"sku": "A1", "warehouse": "w1"})); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEvent(db, reg, EventLike{Type: "SkuDiscontinued", StreamID: "catalog-7", Tenant: "acme", Payload: map[string]any{"sku": "A1"}}); err != nil {
		t.Fatalf("delete fold: %v", err)
	}
	if _, ok := stockRow(t, db, "A1"); ok {
		t.Fatal("delete keyed by $.sku must remove the A1 row")
	}
}

// No key is the existing contract: the row is the stream's.
func TestKeyDefaultsToStreamID(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "notes",
		Columns: map[string]string{"body": "text"},
		On:      map[string]OpRule{"NoteAdded": {Op: "upsert", Set: map[string]any{"body": "$.body"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEvent(db, reg, EventLike{Type: "NoteAdded", StreamID: "n1", Tenant: "acme", Payload: map[string]any{"body": "x"}}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM read_notes WHERE tenant = ? AND id = ?`, "acme", "n1").Scan(&body); err != nil {
		t.Fatalf("an unkeyed rule must still write the stream's row: %v", err)
	}
}

// A key that does not resolve has no safe default — falling back to the stream
// id would silently mix two identities in one table.
func TestUnresolvableKeyFailsTheFold(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := ApplyEvent(db, reg, move("order-1", map[string]any{"warehouse": "w1"})); err == nil {
		t.Fatal("expected a fold error when the key path does not resolve")
	}
	if _, ok := stockRow(t, db, "order-1"); ok {
		t.Fatal("a failed fold must leave no row")
	}
}

func TestNullKeyFailsTheFold(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := ApplyEvent(db, reg, move("order-1", map[string]any{"sku": nil, "warehouse": "w1"})); err == nil {
		t.Fatal("expected a fold error for a null key")
	}
}

// An object or array is not an identity.
func TestNonScalarKeyFailsTheFold(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := ApplyEvent(db, reg, move("order-1", map[string]any{"sku": map[string]any{"a": 1}, "warehouse": "w1"})); err == nil {
		t.Fatal("expected a fold error for a non-scalar key")
	}
}

// id is TEXT, so a numeric key is text — and 42 is "42", not "42.000000".
func TestNumericKeyBecomesText(t *testing.T) {
	db, reg := setupKeyed(t)
	if err := ApplyEvent(db, reg, move("order-1", map[string]any{"sku": 42.0, "warehouse": "w1"})); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if _, ok := stockRow(t, db, "42"); !ok {
		t.Fatal(`a numeric key must land on id "42"`)
	}
}

// One projection, one identity scheme. A rule without a key beside rules with
// one would put two kinds of row in the same table.
func TestMixedKeyingRejectedAtRegistration(t *testing.T) {
	_, reg := setupKeyed(t)
	err := reg.SaveProjection(&ProjectionDef{
		Name:    "mixed",
		Columns: map[string]string{"n": "integer"},
		On: map[string]OpRule{
			"ThingHappened": {Op: "upsert", Key: "$.sku", Inc: map[string]any{"n": 1}},
			"ThingRemoved":  {Op: "delete"},
		},
	})
	if err == nil {
		t.Fatal("expected registration to reject a projection that keys some rules and not others")
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
}

func TestKeyMustBeAPayloadPath(t *testing.T) {
	_, reg := setupKeyed(t)
	err := reg.SaveProjection(&ProjectionDef{
		Name:    "badkey",
		Columns: map[string]string{"n": "integer"},
		On:      map[string]OpRule{"ThingHappened": {Op: "upsert", Key: "sku", Inc: map[string]any{"n": 1}}},
	})
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("expected ValidationError for a key that is not a $. path, got %T: %v", err, err)
	}
}

// Rebuild is the repair mechanism; payload-keyed rows must come back the same.
func TestRebuildIsDeterministicWithPayloadKeys(t *testing.T) {
	db, reg := setupKeyed(t)
	events := []EventLike{
		move("order-1", map[string]any{"sku": "A1", "warehouse": "w1"}),
		move("shipment-9", map[string]any{"sku": "A1", "warehouse": "w1"}),
		move("order-2", map[string]any{"sku": "B2", "warehouse": "w2"}),
	}
	for _, e := range events {
		if err := ApplyEvent(db, reg, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildProjection(db, reg, "stock", "acme", events); err != nil {
		t.Fatal(err)
	}
	if n, _ := stockRow(t, db, "A1"); n != 2 {
		t.Fatalf("A1 after rebuild: got %d, want 2", n)
	}
	if n, _ := stockRow(t, db, "B2"); n != 1 {
		t.Fatalf("B2 after rebuild: got %d, want 1", n)
	}
}
