// Package readmodel is the mode-A rules + query engine (ADR-006: raw
// parameterized SQL, no ORM). Security invariants, each with a conformance
// check: (1) names resolve only through the registry — _ tables unreachable;
// (2) deny-by-default; (3) tenant = ? AND-ed structurally; (4) client JSON
// never reaches SQL as text — identifiers are whitelisted, values are params.
package readmodel

import (
	"database/sql"
	"errors"
	"regexp"
)

// SQLDB is the minimal SQL surface the engine needs — *sql.DB satisfies it,
// and a future Postgres/dialect adapter can too (ADR-006).
type SQLDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// identifierRe is the ONLY shape a name may take before it can reach an SQL
// identifier position. Also why _-prefixed physical tables are unreachable.
var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// eventTypeRe — PascalCase past-tense fact.
var eventTypeRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]+$`)

// sortRe — an optional leading '-' then an identifier.
var sortRe = regexp.MustCompile(`^-?[a-z][a-z0-9_]*$`)

func isIdentifier(s string) bool { return identifierRe.MatchString(s) }

// Typed errors — the host maps them to HTTP status codes.
var (
	ErrNotFound   = errors.New("no such projection")
	ErrForbidden  = errors.New("forbidden")
	ErrValidation = errors.New("invalid query")
)

// NotFoundError / ForbiddenError / ValidationError carry a message.
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string { return e.Msg }

type ForbiddenError struct{ Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ColType is a read-model column's storage type.
type ColType string

const (
	ColText    ColType = "text"
	ColInteger ColType = "integer"
	ColReal    ColType = "real"
)

func validColType(t string) bool {
	return t == string(ColText) || t == string(ColInteger) || t == string(ColReal)
}

// OpRule is one event-type rule inside a projection.
type OpRule struct {
	Op  string         `json:"op"`
	Set map[string]any `json:"set,omitempty"`
	Inc map[string]float64 `json:"inc,omitempty"`
}

// ProjectionDef — a row in _projections; rules as data.
type ProjectionDef struct {
	Name    string             `json:"name"`
	Table   string             `json:"table,omitempty"`
	Columns map[string]string  `json:"columns"`
	On      map[string]OpRule  `json:"on"`
}

// TableOf returns the physical table for a definition.
func (d *ProjectionDef) TableOf() string {
	if d.Table != "" {
		return d.Table
	}
	return "read_" + d.Name
}

// ValidateProjection enforces the definition schema (the zod equivalent).
func ValidateProjection(d *ProjectionDef) error {
	if !isIdentifier(d.Name) {
		return &ValidationError{"projection name must be a lowercase identifier"}
	}
	if d.Table != "" && !isIdentifier(d.Table) {
		return &ValidationError{"table must be a lowercase identifier"}
	}
	for col, typ := range d.Columns {
		if !isIdentifier(col) {
			return &ValidationError{"column name must be a lowercase identifier: " + col}
		}
		if !validColType(typ) {
			return &ValidationError{"column type must be text|integer|real: " + col}
		}
	}
	for evt, rule := range d.On {
		if !eventTypeRe.MatchString(evt) {
			return &ValidationError{"event type must be PascalCase: " + evt}
		}
		if rule.Op != "upsert" && rule.Op != "delete" {
			return &ValidationError{"rule op must be upsert|delete"}
		}
		for c := range rule.Set {
			if !isIdentifier(c) {
				return &ValidationError{"set column must be a lowercase identifier: " + c}
			}
		}
		for c := range rule.Inc {
			if !isIdentifier(c) {
				return &ValidationError{"inc column must be a lowercase identifier: " + c}
			}
		}
	}
	return nil
}

// PolicyDef — a row in _policies. Deny-by-default.
type PolicyDef struct {
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Action string   `json:"action,omitempty"`
	Using  string   `json:"using,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// ValidatePolicy enforces the policy schema.
func ValidatePolicy(p *PolicyDef) error {
	if !isIdentifier(p.Name) {
		return &ValidationError{"policy name must be a lowercase identifier"}
	}
	if p.Role == "" {
		return &ValidationError{"policy role is required"}
	}
	if p.Action == "" {
		p.Action = "select"
	}
	if p.Action != "select" {
		return &ValidationError{"policy action must be select"}
	}
	for _, c := range append(append([]string{}, p.Allow...), p.Deny...) {
		if !isIdentifier(c) {
			return &ValidationError{"allow/deny column must be a lowercase identifier: " + c}
		}
	}
	return nil
}

// AuthCtx binds into policy :auth_* placeholders. Tenant is always structural.
type AuthCtx struct {
	Tenant string
	UID    string
	Role   string
	Email  string
	Claims map[string]any
}

// EventLike is the minimal event surface the projector folds.
type EventLike struct {
	Type     string
	StreamID string
	Tenant   string
	Payload  map[string]any
}
