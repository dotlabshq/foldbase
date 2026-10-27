package flectdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSqld answers the Hrana-over-HTTP pipeline the libsql driver speaks, just
// well enough for a statement to succeed, and records the credentials each
// request carried. It stands in for sqld or for the Flect data gateway.
type fakeSqld struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newFakeSqld(t *testing.T) *fakeSqld {
	t.Helper()
	f := &fakeSqld{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Header.Clone())
		f.mu.Unlock()
		var req struct {
			Requests []struct {
				Type string `json:"type"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := []map[string]any{}
		for _, q := range req.Requests {
			resp := map[string]any{"type": q.Type}
			if q.Type == "execute" {
				resp["result"] = map[string]any{"cols": []any{}, "rows": []any{}, "affected_row_count": 0, "last_insert_rowid": nil}
			}
			results = append(results, map[string]any{"type": "ok", "response": resp})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"baton": "b", "results": results})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSqld) last(t *testing.T) http.Header {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("the database was never called")
	}
	return f.seen[len(f.seen)-1]
}

// fakeBroker serves GET /v1/bindings/{name}/resolve. Each call hands out the
// manifest next() returns, so a test can rotate credentials.
type fakeBroker struct {
	*httptest.Server
	calls atomic.Int32
	down  atomic.Bool
	auth  atomic.Value // last Authorization header
	path  atomic.Value // last request path
}

func newFakeBroker(t *testing.T, next func(n int) any) *fakeBroker {
	t.Helper()
	b := &fakeBroker{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(b.calls.Add(1))
		b.auth.Store(r.Header.Get("Authorization"))
		b.path.Store(r.Method + " " + r.URL.Path)
		if b.down.Load() {
			http.Error(w, `{"error":{"code":"UNAVAILABLE"}}`, 503)
			return
		}
		body := next(n)
		if status, ok := body.(int); ok {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"DENIED","message":"no grant"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(b.Close)
	return b
}

// clock is a settable time source for expiry decisions.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func manifestJSON(conn map[string]any, expires time.Time) map[string]any {
	return map[string]any{
		"version": 1, "binding": "DB", "kind": "database", "provider": "libsql",
		"connection": conn, "capabilities": []string{"read", "write"},
		"expiresAt": expires.Format(time.RFC3339),
	}
}

func wrapped(m map[string]any) map[string]any { return map[string]any{"manifest": m} }

func config(b *fakeBroker, c *clock) Config {
	return Config{BrokerURL: b.URL, Token: "app-token", Now: c.Now, Attempts: 2, Backoff: time.Millisecond}
}

// open opens the binding with no idle pool, so every statement asks the
// connector for a fresh connection — the moment credentials are chosen.
func open(t *testing.T, cfg Config) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), "DB", cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS t (x)"); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func TestBindingParsesTheDBURL(t *testing.T) {
	for in, want := range map[string]string{"flect://DB": "DB", "flect://foldbase-db": "foldbase-db"} {
		if got, err := Binding(in); err != nil || got != want {
			t.Errorf("Binding(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"flect://", "flect://DB/x", "flect://DB?ns=other", "flect://user@DB", "libsql://DB"} {
		if _, err := Binding(bad); err == nil {
			t.Errorf("Binding(%q) accepted; want an error", bad)
		}
	}
}

func TestConfigFromEnvNeedsTheBrokerAndToken(t *testing.T) {
	t.Setenv("FLECT_BROKER_URL", "")
	t.Setenv("FLECT_TOKEN", "x")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("no FLECT_BROKER_URL accepted")
	}
	t.Setenv("FLECT_BROKER_URL", "http://broker")
	t.Setenv("FLECT_TOKEN", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("no FLECT_TOKEN accepted")
	}
	t.Setenv("FLECT_TOKEN", "x")
	if c, err := ConfigFromEnv(); err != nil || c.BrokerURL != "http://broker" || c.Token != "x" {
		t.Errorf("ConfigFromEnv = %+v, %v", c, err)
	}
}

// Gateway mode: the manifest names the gateway and a data token. The token is
// the credential; the namespace is the gateway's business, so none is sent.
func TestGatewayManifestSendsTheTokenAndNoNamespace(t *testing.T) {
	db := newFakeSqld(t)
	c := &clock{now: t0}
	b := newFakeBroker(t, func(int) any {
		return wrapped(manifestJSON(map[string]any{"url": db.URL, "authToken": "data-token"}, t0.Add(15*time.Minute)))
	})
	exec(t, open(t, config(b, c)))

	if got := b.path.Load(); got != "GET /v1/bindings/DB/resolve" {
		t.Errorf("broker call = %v", got)
	}
	if got := b.auth.Load(); got != "Bearer app-token" {
		t.Errorf("broker auth = %v; want the app's FLECT_TOKEN", got)
	}
	h := db.last(t)
	if got := h.Get("Authorization"); got != "Bearer data-token" {
		t.Errorf("database auth = %q; want the manifest's data token", got)
	}
	if got := h.Get("X-Namespace"); got != "" {
		t.Errorf("x-namespace = %q; want none in gateway mode", got)
	}
}

// Direct mode: the broker names the namespace, and only the broker does.
func TestDirectManifestStampsTheBrokersNamespace(t *testing.T) {
	db := newFakeSqld(t)
	c := &clock{now: t0}
	b := newFakeBroker(t, func(int) any {
		return manifestJSON(map[string]any{"url": db.URL, "namespace": "acme-clave-db-1a2b3c4d"}, t0.Add(15*time.Minute)) // bare, not wrapped
	})
	exec(t, open(t, config(b, c)))

	h := db.last(t)
	if got := h.Get("X-Namespace"); got != "acme-clave-db-1a2b3c4d" {
		t.Errorf("x-namespace = %q; want the manifest's", got)
	}
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("database auth = %q; the manifest gave none", got)
	}
}

// Credentials are renewed before they expire, not after: once two thirds of
// the manifest's life has passed, the next connection resolves again.
func TestCredentialsAreRenewedBeforeTheyExpire(t *testing.T) {
	db := newFakeSqld(t)
	c := &clock{now: t0}
	b := newFakeBroker(t, func(n int) any {
		return wrapped(manifestJSON(map[string]any{"url": db.URL, "authToken": "token-" + string(rune('0'+n))},
			c.Now().Add(15*time.Minute)))
	})
	conn := open(t, config(b, c))

	exec(t, conn)
	if got := db.last(t).Get("Authorization"); got != "Bearer token-1" {
		t.Fatalf("first token = %q", got)
	}
	c.Add(9 * time.Minute) // 6 of 15 minutes left: still fresh
	exec(t, conn)
	if got := db.last(t).Get("Authorization"); got != "Bearer token-1" {
		t.Errorf("at 9m = %q; want token-1 kept", got)
	}
	c.Add(2 * time.Minute) // 4 of 15 left: under a third, renew
	exec(t, conn)
	if got := db.last(t).Get("Authorization"); got != "Bearer token-2" {
		t.Errorf("at 11m = %q; want token-2", got)
	}
}

// The pool must not keep a connection, and so a token, past the point where
// it would be renewed: a connection opened just before renewal still has a
// third of the token's life, so it has to close within that.
func TestPooledConnectionsDoNotOutliveTheirToken(t *testing.T) {
	for _, ttl := range []time.Duration{time.Minute, 15 * time.Minute, 24 * time.Hour} {
		if lt := connLifetime(ttl); lt <= 0 || lt >= ttl/3 {
			t.Errorf("connLifetime(%v) = %v; want positive and under %v", ttl, lt, ttl/3)
		}
	}
}

// A broker outage must not take the database down while the credentials in
// hand are still valid; once they have expired there is nothing to use.
func TestBrokerOutageKeepsValidCredentialsUntilTheyExpire(t *testing.T) {
	db := newFakeSqld(t)
	c := &clock{now: t0}
	b := newFakeBroker(t, func(int) any {
		return wrapped(manifestJSON(map[string]any{"url": db.URL, "authToken": "t1"}, c.Now().Add(15*time.Minute)))
	})
	conn := open(t, config(b, c))
	exec(t, conn)

	b.down.Store(true)
	c.Add(12 * time.Minute) // renewal due, broker down, token still valid
	exec(t, conn)
	if got := db.last(t).Get("Authorization"); got != "Bearer t1" {
		t.Errorf("during outage = %q; want the still-valid t1", got)
	}

	c.Add(4 * time.Minute) // past expiry
	if _, err := conn.Exec("SELECT 1"); err == nil {
		t.Error("an expired manifest was used while the broker was down")
	}

	b.down.Store(false)
	if _, err := conn.Exec("SELECT 1"); err != nil {
		t.Errorf("after the broker came back: %v", err)
	}
}

func TestBootFailsClearlyWhenTheBrokerRefuses(t *testing.T) {
	c := &clock{now: t0}
	b := newFakeBroker(t, func(int) any { return 403 })
	_, err := Open(context.Background(), "DB", config(b, c))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v; want the broker's 403", err)
	}
	if n := b.calls.Load(); n != 1 {
		t.Errorf("a refusal was retried %d times; only an outage is worth retrying", n)
	}
}

func TestBootRetriesAnOutageThenGivesUp(t *testing.T) {
	c := &clock{now: t0}
	b := newFakeBroker(t, func(int) any { return nil })
	b.down.Store(true)
	cfg := config(b, c)
	cfg.Attempts = 3
	if _, err := Open(context.Background(), "DB", cfg); err == nil {
		t.Fatal("opened with the broker down")
	}
	if n := b.calls.Load(); n != 3 {
		t.Errorf("broker called %d times; want 3 attempts", n)
	}
}

func TestBootSurvivesABriefOutage(t *testing.T) {
	db := newFakeSqld(t)
	c := &clock{now: t0}
	var b *fakeBroker
	b = newFakeBroker(t, func(n int) any {
		return wrapped(manifestJSON(map[string]any{"url": db.URL, "authToken": "t"}, t0.Add(15*time.Minute)))
	})
	b.down.Store(true)
	go func() { time.Sleep(20 * time.Millisecond); b.down.Store(false) }()
	cfg := config(b, c)
	cfg.Attempts, cfg.Backoff = 20, 5*time.Millisecond
	exec(t, open(t, cfg))
}

// A manifest foldbase cannot use correctly is refused at boot rather than half
// honoured.
func TestUnusableManifestsAreRefused(t *testing.T) {
	future := t0.Add(15 * time.Minute)
	ok := func() map[string]any { return manifestJSON(map[string]any{"url": "http://sqld:8080"}, future) }
	cases := map[string]func(m map[string]any){
		"unknown version":   func(m map[string]any) { m["version"] = 2 },
		"not a database":    func(m map[string]any) { m["kind"] = "kv" },
		"postgres provider": func(m map[string]any) { m["provider"] = "postgres" },
		"no url":            func(m map[string]any) { m["connection"] = map[string]any{} },
		"no expiry":         func(m map[string]any) { delete(m, "expiresAt") },
		"already expired":   func(m map[string]any) { m["expiresAt"] = t0.Add(-time.Second).Format(time.RFC3339) },
		"namespace over ws": func(m map[string]any) {
			m["connection"] = map[string]any{"url": "ws://sqld:8080", "namespace": "n"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := ok()
			mutate(m)
			b := newFakeBroker(t, func(int) any { return wrapped(m) })
			if _, err := Open(context.Background(), "DB", config(b, &clock{now: t0})); err == nil {
				t.Errorf("accepted a manifest with %s", name)
			}
		})
	}
}
