// foldbase (Go) — the append-only event log + read models over HTTP.
// Behavior is defined by openapi.yaml and locked by conformance/run.mjs; this
// binary must green every conformance check the TS reference greens, on both
// SQLite and PostgreSQL (ADR-010).
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"                  // "pgx" driver for database/sql
	_ "github.com/tursodatabase/libsql-client-go/libsql" // "libsql" driver (Turso / sqld, pure-Go, no CGO)
	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/auth"
	"github.com/dotlabshq/foldbase/internal/dialect"
	"github.com/dotlabshq/foldbase/internal/httpapi"
	"github.com/dotlabshq/foldbase/internal/readmodel"
	"github.com/dotlabshq/foldbase/internal/store"
)

func main() {
	// Fail closed on auth misconfiguration BEFORE opening a database (ADR-002).
	if err := auth.AssertConfig(); err != nil {
		log.Fatalf("[foldbase] boot refused: %v", err)
	}

	conn, label, err := openDB()
	if err != nil {
		log.Fatalf("[foldbase] database: %v", err)
	}
	defer conn.Close()

	// Event-log schema + read-model registry (idempotent DDL on boot). The DDL
	// is the one thing that genuinely differs per dialect (autoincrement, types).
	for _, stmt := range strings.Split(conn.D.EventsSchema(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.DB.Exec(stmt); err != nil {
			log.Fatalf("[foldbase] schema: %v", err)
		}
	}
	// Best-effort in-place upgrade for pre-stream_type databases.
	_, _ = conn.DB.Exec(store.MigrateSQL)

	st := store.New(conn)
	reg := readmodel.NewRegistry(conn, conn.D)
	if err := reg.Init(); err != nil {
		log.Fatalf("[foldbase] registry: %v", err)
	}

	handler := httpapi.New(st, reg, conn)

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
	log.Printf("[foldbase] Database: %s (%s)", label, conn.D)
	log.Printf("[foldbase] Projections: %s", strings.Join(names, ", "))

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("[foldbase] serve: %v", err)
	}
}

// openDB resolves DB_URL to a dialect-aware Conn:
//
//	postgres:// | postgresql://                 → PostgreSQL (pgx)
//	libsql:// | ws(s):// | http(s)://            → Turso / sqld remote (libsql, Hrana; pure-Go)
//	:memory: | file:<path>                       → embedded SQLite (modernc, CGO-free)
//
// libsql speaks the SQLite dialect end-to-end (same SQL, LastInsertId, PRAGMA),
// so a remote sqld is just the SQLite dialect over the network.
func openDB() (*dialect.Conn, string, error) {
	url := os.Getenv("DB_URL")
	if url == "" {
		url = ":memory:"
	}

	switch {
	case strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://"):
		db, err := sql.Open("pgx", url)
		if err != nil {
			return nil, "", err
		}
		if err := db.Ping(); err != nil {
			return nil, "", fmt.Errorf("postgres: %w", err)
		}
		return dialect.New(db, dialect.Dialect{Kind: dialect.Postgres}), redact(url), nil

	case isLibsqlURL(url):
		db, err := sql.Open("libsql", url)
		if err != nil {
			return nil, "", err
		}
		if err := db.Ping(); err != nil {
			return nil, "", fmt.Errorf("libsql: %w", err)
		}
		return dialect.New(db, dialect.Dialect{Kind: dialect.SQLite}), redact(url), nil

	case url == ":memory:":
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, "", err
		}
		// One shared connection keeps the in-memory DB alive and single-writer.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		return dialect.New(db, dialect.Dialect{Kind: dialect.SQLite}), ":memory:", nil

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
		return dialect.New(db, dialect.Dialect{Kind: dialect.SQLite}), url, nil

	default:
		return nil, "", fmt.Errorf("unsupported DB_URL %q (postgres://, :memory:, or file:)", url)
	}
}

// isLibsqlURL reports whether DB_URL names a Turso/sqld remote. These schemes
// are unambiguous for a database url (postgres uses postgres://, embedded
// SQLite uses file:/:memory:), so an http:// DB_URL can only mean a sqld HTTP
// endpoint.
func isLibsqlURL(u string) bool {
	for _, p := range []string{"libsql://", "wss://", "ws://", "https://", "http://"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// redact hides the password / auth token in a database URL for logging.
func redact(url string) string {
	if i := strings.Index(url, "authToken="); i >= 0 {
		end := strings.IndexAny(url[i:], "&")
		if end < 0 {
			url = url[:i] + "authToken=***"
		} else {
			url = url[:i] + "authToken=***" + url[i+end:]
		}
	}
	if at := strings.LastIndex(url, "@"); at > 0 {
		if slash := strings.Index(url, "://"); slash > 0 {
			return url[:slash+3] + "***@" + url[at+1:]
		}
	}
	return url
}
