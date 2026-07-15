package dialect

import (
	"errors"
	"testing"
)

func TestRewrite(t *testing.T) {
	pg := Dialect{Kind: Postgres}
	sqlite := Dialect{Kind: SQLite}
	q := "SELECT * FROM t WHERE a = ? AND b IN (?, ?)"
	if got := pg.Rewrite(q); got != "SELECT * FROM t WHERE a = $1 AND b IN ($2, $3)" {
		t.Fatalf("pg rewrite wrong: %s", got)
	}
	if got := sqlite.Rewrite(q); got != q {
		t.Fatalf("sqlite should not rewrite: %s", got)
	}
}

func TestColumnType(t *testing.T) {
	pg := Dialect{Kind: Postgres}
	sqlite := Dialect{Kind: SQLite}
	// integer MUST be 64-bit on Postgres (epoch-ms timestamps overflow INTEGER).
	if pg.ColumnType("integer") != "BIGINT" || pg.ColumnType("real") != "DOUBLE PRECISION" || pg.ColumnType("text") != "TEXT" {
		t.Fatal("pg column types wrong")
	}
	if sqlite.ColumnType("integer") != "INTEGER" || sqlite.ColumnType("real") != "REAL" {
		t.Fatal("sqlite column types wrong")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	cases := map[string]bool{
		"UNIQUE constraint failed: events.id":                    true,
		"ERROR: duplicate key value violates unique constraint": true,
		"SQLSTATE 23505":                                         true,
		"some other error":                                      false,
	}
	for msg, want := range cases {
		if got := IsUniqueViolation(errors.New(msg)); got != want {
			t.Fatalf("IsUniqueViolation(%q) = %v, want %v", msg, got, want)
		}
	}
	if IsUniqueViolation(nil) {
		t.Fatal("nil should not be a unique violation")
	}
}
