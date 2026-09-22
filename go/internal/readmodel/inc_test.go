package readmodel

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/dialect"
)

// setupInc registers a stock projection whose counter is fed by a payload path
// rather than a literal, plus a literal-fed counter to prove that still works.
func setupInc(t *testing.T) (*sql.DB, *Registry) {
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
		Columns: map[string]string{"on_hand": "real", "moves": "integer"},
		On: map[string]OpRule{
			"StockMoved": {Op: "upsert", Inc: map[string]any{"on_hand": "$.quantity", "moves": 1}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return db, reg
}

func onHand(t *testing.T, db *sql.DB, id string) (any, bool) {
	t.Helper()
	var v any
	err := db.QueryRow(`SELECT on_hand FROM read_stock WHERE tenant = ? AND id = ?`, "acme", id).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, true
}

func moved(id string, payload map[string]any) EventLike {
	return EventLike{Type: "StockMoved", StreamID: id, Tenant: "acme", Payload: payload}
}

// inc must add the number the event carries, not just a literal — negatives
// included, which is what a shipment needs.
func TestIncResolvesPayloadPath(t *testing.T) {
	db, reg := setupInc(t)
	for _, q := range []float64{10, -3} {
		if err := ApplyEvent(db, reg, moved("a1", map[string]any{"quantity": q})); err != nil {
			t.Fatalf("fold: %v", err)
		}
	}
	got, ok := onHand(t, db, "a1")
	if !ok {
		t.Fatal("no row")
	}
	if n, _ := got.(float64); n != 7 {
		t.Fatalf("on_hand: got %#v, want 7", got)
	}
}

// A literal inc is the existing contract and must be untouched.
func TestIncLiteralStillCounts(t *testing.T) {
	db, reg := setupInc(t)
	for i := 0; i < 3; i++ {
		if err := ApplyEvent(db, reg, moved("a1", map[string]any{"quantity": 1.0})); err != nil {
			t.Fatalf("fold: %v", err)
		}
	}
	var moves int64
	if err := db.QueryRow(`SELECT moves FROM read_stock WHERE tenant = ? AND id = ?`, "acme", "a1").Scan(&moves); err != nil {
		t.Fatal(err)
	}
	if moves != 3 {
		t.Fatalf("moves: got %d, want 3", moves)
	}
}

// A field the definition names but the payload does not carry is a definition
// bug. It must fail the fold (→ projected:false, healed by a rebuild) rather
// than write a 0 that reads like a real total.
func TestIncMissingFieldFailsTheFold(t *testing.T) {
	db, reg := setupInc(t)
	err := ApplyEvent(db, reg, moved("a1", map[string]any{"qty": 10.0}))
	if err == nil {
		t.Fatal("expected a fold error for a path the payload does not carry")
	}
	if _, ok := onHand(t, db, "a1"); ok {
		t.Fatal("a failed fold must not leave a row behind")
	}
}

// An explicit null is the app saying "no movement on this one" — deliberate,
// and distinguishable from a missing field. It counts as zero.
func TestIncExplicitNullCountsAsZero(t *testing.T) {
	db, reg := setupInc(t)
	if err := ApplyEvent(db, reg, moved("a1", map[string]any{"quantity": nil})); err != nil {
		t.Fatalf("explicit null must not fail the fold: %v", err)
	}
	got, ok := onHand(t, db, "a1")
	if !ok {
		t.Fatal("no row")
	}
	if n, _ := got.(float64); n != 0 {
		t.Fatalf("on_hand: got %#v, want 0", got)
	}
}

// A type mismatch is always a bug, never a silent zero.
func TestIncNonNumericFailsTheFold(t *testing.T) {
	db, reg := setupInc(t)
	if err := ApplyEvent(db, reg, moved("a1", map[string]any{"quantity": "10"})); err == nil {
		t.Fatal("expected a fold error for a non-numeric inc value")
	}
}

// An inc value is a number or a payload path. Anything else is caught when the
// definition is registered, not when an event happens to arrive.
func TestIncValueValidatedAtRegistration(t *testing.T) {
	_, reg := setupInc(t)
	err := reg.SaveProjection(&ProjectionDef{
		Name:    "bad",
		Columns: map[string]string{"n": "integer"},
		On:      map[string]OpRule{"Thing": {Op: "upsert", Inc: map[string]any{"n": "quantity"}}},
	})
	if err == nil {
		t.Fatal("expected registration to reject an inc value that is neither a number nor a $. path")
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
}

// A rebuild must survive an event the definition cannot fold. Iron rule 1 says
// a fold failure may only leave a view stale, repaired by replaying the log —
// a rebuild that dies on one bad event takes the repair away too, and with it
// the whole recovery story for a mistyped path.
func TestRebuildSkipsUnfoldableEventsAndCountsThem(t *testing.T) {
	db, reg := setupInc(t)
	events := []EventLike{
		moved("a1", map[string]any{"quantity": 10.0}),
		moved("a2", map[string]any{"qty": 5.0}), // the definition cannot fold this one
		moved("a3", map[string]any{"quantity": 2.0}),
	}
	skipped, err := RebuildProjection(db, reg, "stock", "acme", events)
	if err != nil {
		t.Fatalf("a rebuild must not abandon the view over one unfoldable event: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped: got %d, want 1", skipped)
	}
	for _, id := range []string{"a1", "a3"} {
		if _, ok := onHand(t, db, id); !ok {
			t.Fatalf("%s is foldable and should be in the view", id)
		}
	}
	if _, ok := onHand(t, db, "a2"); ok {
		t.Fatal("a2 could not be folded and must leave no row")
	}
}

// "$." with nothing after it, or an empty segment, names no field. Rejecting
// those at registration keeps the failure where the mistake is, instead of one
// fold error per event forever.
func TestIncPathGrammarValidatedAtRegistration(t *testing.T) {
	_, reg := setupInc(t)
	for _, bad := range []string{"$.", "$.a..b", "$..a", "$.a."} {
		err := reg.SaveProjection(&ProjectionDef{
			Name:    "badpath",
			Columns: map[string]string{"n": "integer"},
			On:      map[string]OpRule{"Thing": {Op: "upsert", Inc: map[string]any{"n": bad}}},
		})
		if _, ok := err.(*ValidationError); !ok {
			t.Fatalf("inc path %q: expected ValidationError, got %T: %v", bad, err, err)
		}
	}
	// A nested path is a real path and stays valid.
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "nested",
		Columns: map[string]string{"n": "integer"},
		On:      map[string]OpRule{"Thing": {Op: "upsert", Inc: map[string]any{"n": "$.line.qty"}}},
	}); err != nil {
		t.Fatalf("a nested inc path must stay valid: %v", err)
	}
}

// The server resolves a nested inc path, so the authoring layers must not
// declare it as a text column (see the client tests).
func TestIncResolvesANestedPath(t *testing.T) {
	db, reg := setupInc(t)
	if err := reg.SaveProjection(&ProjectionDef{
		Name:    "nested",
		Columns: map[string]string{"n": "integer"},
		On:      map[string]OpRule{"Thing": {Op: "upsert", Inc: map[string]any{"n": "$.line.qty"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEvent(db, reg, EventLike{Type: "Thing", StreamID: "s1", Tenant: "acme",
		Payload: map[string]any{"line": map[string]any{"qty": 4.0}}}); err != nil {
		t.Fatalf("fold: %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT n FROM read_nested WHERE tenant = ? AND id = ?`, "acme", "s1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("n: got %d, want 4", n)
	}
}
