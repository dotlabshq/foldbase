package store

import (
	"database/sql"
	"regexp"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func ev(typ string) NewEvent {
	return NewEvent{Type: typ, StreamID: "s1", Actor: "t", Payload: map[string]any{"k": "v"}}
}

func TestAppendAssignsVersionsAndGlobalSeq(t *testing.T) {
	s := newTestStore(t)
	r, err := s.Append("acme", "s1", "", 0, []NewEvent{ev("AThing"), ev("BThing")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 2 || r.Events[0].Version != 1 || r.Events[1].Version != 2 {
		t.Fatalf("bad versions: %+v", r)
	}
	if r.Events[1].GlobalSeq <= r.Events[0].GlobalSeq {
		t.Fatalf("globalSeq not monotonic")
	}
}

func TestAppendConcurrencyConflict(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Append("acme", "s1", "", 0, []NewEvent{ev("AThing")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append("acme", "s1", "", 0, []NewEvent{ev("AThing")})
	ce, ok := err.(*ConcurrencyError)
	if !ok || ce.Actual != 1 {
		t.Fatalf("expected ConcurrencyError{Actual:1}, got %v", err)
	}
}

func TestStreamTypeFixedByFirstAppend(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Append("acme", "s1", "task", 0, []NewEvent{ev("AThing")}); err != nil {
		t.Fatal(err)
	}
	// conflicting non-empty type → error
	if _, err := s.Append("acme", "s1", "other", 1, []NewEvent{ev("AThing")}); err == nil {
		t.Fatal("expected StreamTypeError")
	} else if _, ok := err.(*StreamTypeError); !ok {
		t.Fatalf("expected StreamTypeError, got %v", err)
	}
	// omitted type inherits the existing one
	r, err := s.Append("acme", "s1", "", 1, []NewEvent{ev("AThing")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Events[0].StreamType != "task" {
		t.Fatalf("expected inherited streamType 'task', got %q", r.Events[0].StreamType)
	}
}

func TestEventIDIsUUIDv7AndClientIDHonored(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.Append("acme", "s1", "", 0, []NewEvent{ev("AThing")})
	v7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !v7.MatchString(r.Events[0].ID) {
		t.Fatalf("not a uuidv7: %s", r.Events[0].ID)
	}
	custom := "01920000-0000-7000-8000-00000000abcd"
	e := ev("AThing")
	e.ID = custom
	r2, err := s.Append("acme", "s2", "", 0, []NewEvent{e})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Events[0].ID != custom {
		t.Fatalf("client id not honored: %s", r2.Events[0].ID)
	}
}

func TestReadAllCategoryFilterAndLimit(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("acme", "t1", "task", 0, []NewEvent{{Type: "AThing", StreamID: "t1", Actor: "t"}})
	_, _ = s.Append("acme", "u1", "user", 0, []NewEvent{{Type: "BThing", StreamID: "u1", Actor: "t"}})
	_, _ = s.Append("acme", "t2", "task", 0, []NewEvent{{Type: "AThing", StreamID: "t2", Actor: "t"}})

	tasks, err := s.ReadAll("acme", 0, "task", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 task events, got %d", len(tasks))
	}
	limited, _ := s.ReadAll("acme", 0, "", 1)
	if len(limited) != 1 {
		t.Fatalf("limit not applied: %d", len(limited))
	}
	// tenant isolation
	other, _ := s.ReadAll("globex", 0, "", 0)
	if len(other) != 0 {
		t.Fatalf("tenant isolation broken")
	}
}
