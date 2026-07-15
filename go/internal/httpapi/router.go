// Package httpapi is the HTTP surface — the Go equivalent of the TS Hono app.
// Route classification into data/control planes mirrors ADR-003.
package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"github.com/dotlabshq/foldbase/internal/auth"
	"github.com/dotlabshq/foldbase/internal/readmodel"
	"github.com/dotlabshq/foldbase/internal/store"
)

var eventTypeRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]+$`)
var streamTypeRe = regexp.MustCompile(`^([a-z][a-z0-9_]*)?$`)

// Log reads page with a generous default; explicit limits may raise it.
const defaultReadLimit = 1000
const maxReadLimit = 10000

func readLimit(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return defaultReadLimit
	}
	if n > maxReadLimit {
		return maxReadLimit
	}
	return n
}

// App holds the handler dependencies.
type App struct {
	store *store.Store
	reg   *readmodel.Registry
	db    dbExec
}

// dbExec is the minimal SQL surface the read-model engine needs.
type dbExec = readmodel.SQLDB

// New builds the http.Handler.
func New(st *store.Store, reg *readmodel.Registry, db readmodel.SQLDB) http.Handler {
	a := &App{store: st, reg: reg, db: db}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /v1/streams/{streamId}/version", a.withAuth(a.streamVersion))
	mux.HandleFunc("POST /v1/streams/{streamId}", a.withAuth(a.append))
	mux.HandleFunc("GET /v1/streams/{streamId}", a.withAuth(a.readStream))
	mux.HandleFunc("GET /v1/events/by-correlation/{correlationId}", a.withAuth(a.byCorrelation))
	mux.HandleFunc("GET /v1/events", a.withAuth(a.readAll))
	mux.HandleFunc("POST /v1/query/{name}", a.withAuth(a.query))
	mux.HandleFunc("PUT /v1/projections", a.withAuth(a.putProjection))
	mux.HandleFunc("PUT /v1/policies", a.withAuth(a.putPolicy))
	mux.HandleFunc("POST /admin/reload", a.withAuth(a.adminReload))
	mux.HandleFunc("POST /admin/rebuild", a.withAuth(a.adminRebuild))

	return logging(mux)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": code})
}

type authedHandler func(w http.ResponseWriter, r *http.Request, res *auth.Resolved)

// withAuth resolves the trust boundary once and hands the result to the handler.
func (a *App) withAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, aerr := auth.Resolve(r)
		if aerr != nil {
			writeJSON(w, aerr.Status, map[string]any{"error": aerr.Message})
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[foldbase] panic: %v", rec)
				body := map[string]any{"error": "internal_server_error"}
				if os.Getenv("NODE_ENV") != "production" {
					body["message"] = toStr(rec)
				}
				writeJSON(w, 500, body)
			}
		}()
		h(w, r, res)
	}
}

func toStr(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "unknown error"
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	names := []string{}
	for _, d := range a.reg.ListProjections() {
		names = append(names, d.Name)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "service": "foldbase", "projections": names})
}

func (a *App) streamVersion(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	v, err := a.store.StreamVersion(res.Tenant, r.PathValue("streamId"))
	if err != nil {
		panic(err)
	}
	writeJSON(w, 200, map[string]any{"version": v})
}

type appendBody struct {
	ExpectedVersion *int64           `json:"expectedVersion"`
	StreamType      string           `json:"streamType"`
	Events          []store.NewEvent `json:"events"`
}

func (a *App) append(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	if !res.CanWrite {
		writeErr(w, 403, "append requires a service token")
		return
	}
	var body appendBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "body must be a JSON object"})
		return
	}
	if body.ExpectedVersion == nil || *body.ExpectedVersion < 0 {
		writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "expectedVersion must be a non-negative integer"})
		return
	}
	if len(body.Events) == 0 {
		writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "events must be a non-empty array"})
		return
	}
	if !streamTypeRe.MatchString(body.StreamType) {
		writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "streamType must be a lowercase identifier"})
		return
	}
	for i := range body.Events {
		e := &body.Events[i]
		if !eventTypeRe.MatchString(e.Type) {
			writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "event type must be PascalCase"})
			return
		}
		if e.ID != "" && !store.ValidID(e.ID) {
			writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "id must be a valid UUID"})
			return
		}
		if e.StreamID == "" {
			e.StreamID = r.PathValue("streamId")
		}
		if e.Actor == "" {
			writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": "actor is required"})
			return
		}
		// Stamp the verified writer into metadata for audit (additive).
		if res.Subject != "" {
			if e.Metadata == nil {
				e.Metadata = map[string]any{}
			}
			if _, exists := e.Metadata["writtenBy"]; !exists {
				e.Metadata["writtenBy"] = res.Subject
			}
		}
	}

	result, err := a.store.Append(res.Tenant, r.PathValue("streamId"), body.StreamType, *body.ExpectedVersion, body.Events)
	if err != nil {
		if ce, ok := err.(*store.ConcurrencyError); ok {
			writeJSON(w, 409, map[string]any{"error": "concurrency_conflict", "actual": ce.Actual})
			return
		}
		if te, ok := err.(*store.StreamTypeError); ok {
			writeJSON(w, 400, map[string]any{"error": "invalid_append", "message": te.Error()})
			return
		}
		if store.IsUniqueViolation(err) {
			actual, _ := a.store.StreamVersion(res.Tenant, r.PathValue("streamId"))
			writeJSON(w, 409, map[string]any{"error": "concurrency_conflict", "actual": actual})
			return
		}
		panic(err)
	}

	// Fold AFTER the commit (log-first). A fold failure never fails the append.
	projected := true
	for _, e := range result.Events {
		if ferr := readmodel.ApplyEvent(a.db, a.reg, toEventLike(e)); ferr != nil {
			projected = false
			log.Printf("[foldbase] projection failed (rebuild will heal): %v", ferr)
			break
		}
	}
	writeJSON(w, 200, map[string]any{"events": result.Events, "version": result.Version, "projected": projected})
}

func (a *App) readStream(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	from := int64(0)
	if q := r.URL.Query().Get("fromVersion"); q != "" {
		from, _ = strconv.ParseInt(q, 10, 64)
	}
	evs, err := a.store.ReadStream(res.Tenant, r.PathValue("streamId"), from, readLimit(r.URL.Query().Get("limit")))
	if err != nil {
		panic(err)
	}
	writeJSON(w, 200, evs)
}

func (a *App) byCorrelation(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	evs, err := a.store.ReadByCorrelation(res.Tenant, r.PathValue("correlationId"), readLimit(r.URL.Query().Get("limit")))
	if err != nil {
		panic(err)
	}
	writeJSON(w, 200, evs)
}

// readAll pages the tenant log; ?type= narrows to one stream category.
func (a *App) readAll(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	from := int64(0)
	if q := r.URL.Query().Get("fromGlobalSeq"); q != "" {
		from, _ = strconv.ParseInt(q, 10, 64)
	}
	evs, err := a.store.ReadAll(res.Tenant, from, r.URL.Query().Get("type"), readLimit(r.URL.Query().Get("limit")))
	if err != nil {
		panic(err)
	}
	writeJSON(w, 200, evs)
}

func (a *App) query(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	ctx := readmodel.AuthCtx{Tenant: res.Tenant, UID: res.Ctx.UID, Role: res.Ctx.Role, Email: res.Ctx.Email}
	result, err := readmodel.ExecQuery(a.db, a.reg, r.PathValue("name"), body, ctx)
	if err != nil {
		mapReadModelErr(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *App) putProjection(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	if !res.CanControl {
		writeErr(w, 403, "control plane requires a service token")
		return
	}
	var def readmodel.ProjectionDef
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeErr(w, 400, "invalid_definition")
		return
	}
	if err := a.reg.SaveProjection(&def); err != nil {
		mapReadModelErr(w, err)
		return
	}
	events, err := a.store.ReadAll(res.Tenant, 0, "", 0)
	if err != nil {
		panic(err)
	}
	if err := readmodel.RebuildProjection(a.db, a.reg, def.Name, res.Tenant, toEventLikes(events)); err != nil {
		panic(err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": def.Name, "rebuiltFrom": len(events)})
}

func (a *App) putPolicy(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	if !res.CanControl {
		writeErr(w, 403, "control plane requires a service token")
		return
	}
	var def readmodel.PolicyDef
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeErr(w, 400, "invalid_definition")
		return
	}
	if err := a.reg.SavePolicy(&def); err != nil {
		mapReadModelErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": def.Name, "role": def.Role})
}

func (a *App) adminReload(w http.ResponseWriter, _ *http.Request, res *auth.Resolved) {
	if !res.CanControl {
		writeErr(w, 403, "control plane requires a service token")
		return
	}
	if err := a.reg.Reload(); err != nil {
		panic(err)
	}
	names := []string{}
	for _, d := range a.reg.ListProjections() {
		names = append(names, d.Name)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "projections": names})
}

func (a *App) adminRebuild(w http.ResponseWriter, r *http.Request, res *auth.Resolved) {
	if !res.CanControl {
		writeErr(w, 403, "control plane requires a service token")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	events, err := a.store.ReadAll(res.Tenant, 0, "", 0)
	if err != nil {
		panic(err)
	}
	els := toEventLikes(events)
	if body.Name != "" {
		if a.reg.GetProjection(body.Name) == nil {
			writeJSON(w, 404, map[string]any{"error": "unknown_projection", "name": body.Name})
			return
		}
		if err := readmodel.RebuildProjection(a.db, a.reg, body.Name, res.Tenant, els); err != nil {
			panic(err)
		}
	} else {
		if err := readmodel.RebuildTenant(a.db, a.reg, res.Tenant, els); err != nil {
			panic(err)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "rebuiltFrom": len(events)})
}

func mapReadModelErr(w http.ResponseWriter, err error) {
	switch e := err.(type) {
	case *readmodel.NotFoundError:
		writeJSON(w, 404, map[string]any{"error": "not_found", "message": e.Msg})
	case *readmodel.ForbiddenError:
		writeJSON(w, 403, map[string]any{"error": "forbidden", "message": e.Msg})
	case *readmodel.ValidationError:
		writeJSON(w, 400, map[string]any{"error": "invalid_query", "message": e.Msg})
	default:
		panic(err)
	}
}

func toEventLike(e store.StoredEvent) readmodel.EventLike {
	return readmodel.EventLike{Type: e.Type, StreamID: e.StreamID, Tenant: e.Tenant, Payload: e.Payload}
}

func toEventLikes(evs []store.StoredEvent) []readmodel.EventLike {
	out := make([]readmodel.EventLike, len(evs))
	for i, e := range evs {
		out[i] = toEventLike(e)
	}
	return out
}

// logging is a minimal request logger (hono/logger equivalent).
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
