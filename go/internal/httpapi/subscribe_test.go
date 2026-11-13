package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/dotlabshq/foldbase/internal/dialect"
	"github.com/dotlabshq/foldbase/internal/readmodel"
	"github.com/dotlabshq/foldbase/internal/store"
)

// newTestServer boots the real handler, wired the way main.go wires it, in
// none-mode auth. The database is an in-memory SQLite, or, when
// FOLDBASE_TEST_PG names a Postgres URL, a fresh schema in that database that
// is dropped afterwards (`just test-go-pg` starts a disposable one).
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("FOLDBASE_AUTH", "none")
	var conn *dialect.Conn
	if pg := os.Getenv("FOLDBASE_TEST_PG"); pg != "" {
		conn = openTestPostgres(t, pg)
	} else {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		conn = dialect.New(db, dialect.Dialect{Kind: dialect.SQLite})
	}
	db := conn.DB
	for _, stmt := range strings.Split(conn.D.EventsSchema(), ";") {
		if strings.TrimSpace(stmt) != "" {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := readmodel.NewRegistry(conn, conn.D)
	if err := reg.Init(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(store.New(conn), reg, conn))
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return srv
}

// openTestPostgres creates a schema of its own in the database at url, points
// every pooled connection at it through search_path, and drops it on cleanup.
func openTestPostgres(t *testing.T, url string) *dialect.Conn {
	t.Helper()
	admin, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("fbtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE") //nolint:errcheck
		admin.Close()
	})
	return dialect.New(db, dialect.Dialect{Kind: dialect.Postgres})
}

func do(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	return doAs(t, method, url, map[string]string{"X-Tenant-ID": "acme"}, body)
}

// doAs sends a request with the given headers (tenant, bearer token, …).
func doAs(t *testing.T, method, url string, headers map[string]string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// subscriber is a well-behaved SSE client: it records every globalSeq it is
// handed and, when the server ends the stream, reconnects with Last-Event-ID —
// exactly what ADR-009 asks of a client. Whatever it still lacks at the end was
// never going to arrive.
type subscriber struct {
	mu   sync.Mutex
	seen map[int64]int
	last int64
}

func (s *subscriber) run(ctx context.Context, base string) {
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/subscribe", nil)
		req.Header.Set("X-Tenant-ID", "acme")
		s.mu.Lock()
		if s.last > 0 {
			req.Header.Set("Last-Event-ID", strconv.FormatInt(s.last, 10))
		}
		s.mu.Unlock()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "id: ") {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			s.mu.Lock()
			s.seen[n]++
			s.last = n
			s.mu.Unlock()
		}
		res.Body.Close()
	}
}

// TestSubscribeDeliversEveryEventUnderConcurrentAppends — issue #13.
//
// Appends publish to the bus after their commit, outside any lock, and the
// subscriber drops anything at or below the last globalSeq it sent. So when
// two appends race, the later seq can reach the bus first, and the earlier one
// is then dropped as "overlap". Its SSE id was never sent, so a reconnect
// resumes past it: the event is lost for good.
//
// Half the writers append an event type a projection folds, which puts work
// between that append's commit and its publish; the other half append a type
// nothing folds, so they publish straight away. That is the window.
func TestSubscribeDeliversEveryEventUnderConcurrentAppends(t *testing.T) {
	assertEveryEventDelivered(t, 16, 40)
}

// The same load from one writer, so nothing races. It passing while the
// concurrent case fails is what pins the loss on the race, not on the harness.
func TestSubscribeDeliversEveryEventFromOneWriter(t *testing.T) {
	assertEveryEventDelivered(t, 1, 640)
}

func assertEveryEventDelivered(t *testing.T, writers, perWriter int) {
	t.Helper()
	srv := newTestServer(t)
	cols := map[string]string{}
	set := map[string]any{}
	for i := 0; i < 12; i++ {
		c := fmt.Sprintf("c%d", i)
		cols[c] = "text"
		set[c] = "$.v"
	}
	for i := 0; i < 4; i++ {
		res := do(t, "PUT", srv.URL+"/v1/projections", map[string]any{
			"name": fmt.Sprintf("slow_%d", i), "columns": cols,
			"on": map[string]any{"Slow": map[string]any{"op": "upsert", "set": set}},
		})
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("register: %d", res.StatusCode)
		}
	}

	sub := &subscriber{seen: map[int64]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.run(ctx, srv.URL)
	time.Sleep(100 * time.Millisecond) // let the subscription go live

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			stream := fmt.Sprintf("s-%d", w)
			for v := 0; v < perWriter; v++ {
				typ := "Fast"
				if (w+v)%2 == 0 {
					typ = "Slow"
				}
				res := do(t, "POST", srv.URL+"/v1/streams/"+stream, map[string]any{
					"expectedVersion": v,
					"events":          []map[string]any{{"type": typ, "actor": "t", "payload": map[string]any{"v": "x"}}},
				})
				res.Body.Close()
				if res.StatusCode != 200 {
					t.Errorf("append %s@%d: %d", stream, v, res.StatusCode)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	total := writers * perWriter
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sub.mu.Lock()
		got := len(sub.seen)
		sub.mu.Unlock()
		if got >= total {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	sub.mu.Lock()
	defer sub.mu.Unlock()
	// The log's own sequence is the reference: Postgres may skip identity
	// values (a rolled-back insert burns one), so 1..total is not assumed.
	rows := readSeqs(t, srv.URL)
	if len(rows) != total {
		t.Fatalf("log holds %d events, want %d", len(rows), total)
	}
	var missing, twice []int64
	for _, seq := range rows {
		switch sub.seen[seq] {
		case 0:
			missing = append(missing, seq)
		case 1:
		default:
			twice = append(twice, seq)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("subscriber never received %d of %d events, e.g. globalSeq %v", len(missing), total, missing[:min(len(missing), 10)])
	}
	if len(twice) > 0 {
		t.Fatalf("subscriber received %d events more than once, e.g. globalSeq %v", len(twice), twice[:min(len(twice), 10)])
	}
}

// readSeqs pages the tenant log and returns every globalSeq in it, in order.
func readSeqs(t *testing.T, base string) []int64 {
	t.Helper()
	var out []int64
	from := int64(0)
	for {
		res := do(t, "GET", fmt.Sprintf("%s/v1/events?fromGlobalSeq=%d&limit=10000", base, from), nil)
		var page []struct {
			GlobalSeq int64 `json:"globalSeq"`
		}
		err := json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		if len(page) == 0 {
			return out
		}
		for _, e := range page {
			out = append(out, e.GlobalSeq)
		}
		from = page[len(page)-1].GlobalSeq
	}
}
