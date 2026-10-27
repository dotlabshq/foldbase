// Package flectdb opens a database that a Flect binding names (DB_URL =
// flect://<binding>). Flect gives a container only FLECT_TOKEN and
// FLECT_BROKER_URL, and the credentials it resolves to expire (a data-gateway
// token lives 15 minutes), so the connection details cannot be a fixed env
// var: foldbase asks the broker, and asks again before they run out.
//
//	GET {FLECT_BROKER_URL}/v1/bindings/{binding}/resolve   Bearer FLECT_TOKEN
//	→ { manifest: { version: 1, kind: "database", provider: "libsql",
//	                connection: { url, authToken?, namespace? }, expiresAt } }
//
// Gateway mode hands back the gateway's url and a data token; the gateway
// derives the namespace itself. Direct mode hands back sqld's url and the
// namespace, which goes out as x-namespace, as Flect's own SDK does. The
// namespace only ever comes from the broker: there is no way to name one by
// hand, and keeping one tenant out of another's namespace is Flect's job.
package flectdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tursodatabase/libsql-client-go/libsql"
)

// Binding returns the binding name a flect:// DB_URL names.
func Binding(dbURL string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme != "flect" {
		return "", fmt.Errorf("%q is not a flect://<binding> url", dbURL)
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q: want flect://<binding> and nothing else", dbURL)
	}
	return u.Host, nil
}

// Config says how to reach the broker. Zero fields take the defaults.
type Config struct {
	BrokerURL string
	Token     string
	Client    *http.Client     // default: 10s timeout
	Now       func() time.Time // default: time.Now
	Attempts  int              // resolutions tried at boot; default 5
	Backoff   time.Duration    // first pause between them, doubling; default 1s
}

// ConfigFromEnv reads the two variables Flect injects into every container.
func ConfigFromEnv() (Config, error) {
	c := Config{BrokerURL: os.Getenv("FLECT_BROKER_URL"), Token: os.Getenv("FLECT_TOKEN")}
	if c.BrokerURL == "" || c.Token == "" {
		return Config{}, errors.New("a flect:// DB_URL needs FLECT_BROKER_URL and FLECT_TOKEN, which Flect sets in the container")
	}
	return c, nil
}

func (c Config) withDefaults() Config {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Attempts <= 0 {
		c.Attempts = 5
	}
	if c.Backoff <= 0 {
		c.Backoff = time.Second
	}
	c.BrokerURL = strings.TrimRight(c.BrokerURL, "/")
	return c
}

type manifest struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	Provider   string `json:"provider"`
	Connection struct {
		URL       string `json:"url"`
		AuthToken string `json:"authToken"`
		Namespace string `json:"namespace"`
	} `json:"connection"`
	ExpiresAt time.Time `json:"expiresAt"`

	issuedAt time.Time // when foldbase received it; with ExpiresAt, its lifetime
}

func (m *manifest) validate(now time.Time) error {
	switch {
	case m.Version != 1:
		return fmt.Errorf("manifest version %d; foldbase reads version 1", m.Version)
	case m.Kind != "database":
		return fmt.Errorf("binding is a %q, not a database", m.Kind)
	case m.Provider != "libsql":
		return fmt.Errorf("provider %q is not supported yet; foldbase reads libsql bindings", m.Provider)
	case m.Connection.URL == "":
		return errors.New("manifest has no connection url")
	case m.ExpiresAt.IsZero():
		return errors.New("manifest has no expiresAt")
	case !m.ExpiresAt.After(now):
		return fmt.Errorf("manifest expired at %s", m.ExpiresAt.Format(time.RFC3339))
	}
	// The driver can stamp a header only on HTTP requests; over a websocket
	// the namespace would be dropped and the default database used instead.
	if m.Connection.Namespace != "" && !strings.HasPrefix(m.Connection.URL, "http") && !strings.HasPrefix(m.Connection.URL, "libsql://") {
		return fmt.Errorf("a namespace needs an http(s) or libsql url, not %q", m.Connection.URL)
	}
	return nil
}

// renewDue reports whether less than a third of the manifest's life is left.
func (m *manifest) renewDue(now time.Time) bool {
	return m.ExpiresAt.Sub(now) < m.ExpiresAt.Sub(m.issuedAt)/3
}

// connLifetime bounds how long the pool keeps one connection, and so one
// token. A connection opened just before renewal holds a token with a third
// of its life left, so it must close well within that.
func connLifetime(ttl time.Duration) time.Duration {
	return max(ttl/4, time.Second)
}

// brokerError is a refusal from the broker. Only an outage (5xx, 429) is
// worth retrying; a 4xx will say the same thing next time.
type brokerError struct {
	status int
	body   string
}

func (e *brokerError) Error() string { return fmt.Sprintf("broker answered %d: %s", e.status, e.body) }

func retryable(err error) bool {
	var be *brokerError
	if errors.As(err, &be) {
		return be.status >= 500 || be.status == http.StatusTooManyRequests
	}
	return true // network error or timeout
}

type connector struct {
	cfg     Config
	binding string

	mu sync.Mutex
	m  *manifest
}

func (c *connector) resolve(ctx context.Context) (*manifest, error) {
	endpoint := c.cfg.BrokerURL + "/v1/bindings/" + url.PathEscape(c.binding) + "/resolve"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	res, err := c.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, &brokerError{status: res.StatusCode, body: strings.TrimSpace(string(body))}
	}
	// RFC-0005 wraps the manifest in { manifest }; a bare one is accepted too.
	var wrapper struct {
		Manifest *manifest `json:"manifest"`
	}
	m := &manifest{}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Manifest != nil {
		m = wrapper.Manifest
	} else if err := json.Unmarshal(body, m); err != nil {
		return nil, &brokerError{status: res.StatusCode, body: "not a manifest: " + err.Error()}
	}
	now := c.cfg.Now()
	if err := m.validate(now); err != nil {
		return nil, fmt.Errorf("binding %q: %w", c.binding, err)
	}
	m.issuedAt = now
	return m, nil
}

// current returns credentials to open a connection with, renewing them when
// they are due. A failed renewal keeps the ones in hand while they are valid.
func (c *connector) current(ctx context.Context) (*manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	if c.m != nil && !c.m.renewDue(now) {
		return c.m, nil
	}
	m, err := c.resolve(ctx)
	if err == nil {
		c.m = m
		return m, nil
	}
	if c.m != nil && c.m.ExpiresAt.After(now) {
		log.Printf("[foldbase] renewing binding %q failed, keeping credentials valid until %s: %v",
			c.binding, c.m.ExpiresAt.Format(time.RFC3339), err)
		return c.m, nil
	}
	return nil, err
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	m, err := c.current(ctx)
	if err != nil {
		return nil, err
	}
	var opts []libsql.Option
	if m.Connection.AuthToken != "" {
		opts = append(opts, libsql.WithAuthToken(m.Connection.AuthToken))
	}
	if m.Connection.Namespace != "" {
		opts = append(opts, libsql.WithRequestHeaders(map[string]string{"x-namespace": m.Connection.Namespace}))
	}
	inner, err := libsql.NewConnector(m.Connection.URL, opts...)
	if err != nil {
		return nil, err
	}
	return inner.Connect(ctx)
}

func (c *connector) Driver() driver.Driver { return libsql.Driver{} }

// Open resolves the binding, retrying an outage with backoff, and returns a
// pool that renews its credentials as they age. It does not ping: the caller
// does, as it does for every other database.
func Open(ctx context.Context, binding string, cfg Config) (*sql.DB, error) {
	cfg = cfg.withDefaults()
	c := &connector{cfg: cfg, binding: binding}
	var err error
	pause := cfg.Backoff
	for attempt := 1; ; attempt++ {
		if c.m, err = c.resolve(ctx); err == nil {
			break
		}
		if !retryable(err) || attempt >= cfg.Attempts {
			return nil, fmt.Errorf("resolve binding %q: %w", binding, err)
		}
		log.Printf("[foldbase] resolve binding %q (attempt %d/%d): %v", binding, attempt, cfg.Attempts, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, 10*time.Second)
	}
	db := sql.OpenDB(c)
	db.SetConnMaxLifetime(connLifetime(c.m.ExpiresAt.Sub(c.m.issuedAt)))
	return db, nil
}
