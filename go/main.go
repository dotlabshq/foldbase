// foldbase (Go) — the append-only event log + read models over HTTP.
// Behavior is defined by openapi.yaml and locked by conformance/run.mjs; this
// binary must green every conformance check the TS reference greens.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/auth"
	"github.com/dotlabshq/foldbase/internal/httpapi"
	"github.com/dotlabshq/foldbase/internal/readmodel"
	"github.com/dotlabshq/foldbase/internal/store"
)

func main() {
	// Fail closed on auth misconfiguration BEFORE opening a database (ADR-002).
	if err := auth.AssertConfig(); err != nil {
		log.Fatalf("[foldbase] boot refused: %v", err)
	}

	db, label, err := openDB()
	if err != nil {
		log.Fatalf("[foldbase] database: %v", err)
	}
	defer db.Close()

	// Event-log schema + read-model registry (idempotent DDL on boot).
	if _, err := db.Exec(store.SchemaSQL); err != nil {
		log.Fatalf("[foldbase] schema: %v", err)
	}
	// Best-effort in-place upgrade for pre-stream_type databases (no-op error on fresh ones).
	_, _ = db.Exec(store.MigrateSQL)
	st := store.New(db)
	reg := readmodel.NewRegistry(db)
	if err := reg.Init(); err != nil {
		log.Fatalf("[foldbase] registry: %v", err)
	}

	handler := httpapi.New(st, reg, db)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}
	names := []string{}
	for _, d := range reg.ListProjections() {
		names = append(names, d.Name)
	}
	log.Printf("[foldbase] Listening on port %s", port)
	log.Printf("[foldbase] Auth mode: %s", auth.CurrentMode())
	log.Printf("[foldbase] Database: %s", label)
	log.Printf("[foldbase] Projections: %s", strings.Join(names, ", "))

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("[foldbase] serve: %v", err)
	}
}

// openDB resolves DB_URL (:memory: | file:… ) to a *sql.DB. Remote sqld
// (http/libsql) is a planned dialect adapter (ADR-006), not yet wired in Go.
func openDB() (*sql.DB, string, error) {
	url := os.Getenv("DB_URL")
	if url == "" {
		url = ":memory:"
	}

	switch {
	case url == ":memory:":
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, "", err
		}
		// One shared connection keeps the in-memory DB alive and single-writer.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		return db, ":memory:", nil

	case strings.HasPrefix(url, "file:"):
		path := url[len("file:"):]
		if dir := filepath.Dir(path); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, "", err
		}
		db.SetMaxOpenConns(1) // SQLite: single writer
		return db, url, nil

	default:
		return nil, "", fmt.Errorf("unsupported DB_URL %q (Go build supports :memory: and file: for now)", url)
	}
}
