package readmodel

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dotlabshq/foldbase/internal/dialect"
)

// Registry holds _projections + _policies in memory. Registry membership IS the
// query allowlist — there is no naming-convention discovery.
type Registry struct {
	db          SQLDB
	d           dialect.Dialect
	mu          sync.RWMutex
	projections map[string]*ProjectionDef
	byEventType map[string][]*ProjectionDef
	policies    map[string]*PolicyDef // key: "name role"
}

func NewRegistry(db SQLDB, d dialect.Dialect) *Registry {
	return &Registry{
		db:          db,
		d:           d,
		projections: map[string]*ProjectionDef{},
		byEventType: map[string][]*ProjectionDef{},
		policies:    map[string]*PolicyDef{},
	}
}

// Init applies the registry DDL (idempotent) and loads definitions.
func (r *Registry) Init() error {
	for _, ddl := range r.d.RegistryDDL() {
		if _, err := r.db.Exec(ddl); err != nil {
			return err
		}
	}
	return r.Reload()
}

// Reload re-reads _projections + _policies into memory.
func (r *Registry) Reload() error {
	projections := map[string]*ProjectionDef{}
	byEventType := map[string][]*ProjectionDef{}
	policies := map[string]*PolicyDef{}

	prows, err := r.db.Query(`SELECT def FROM _projections`)
	if err != nil {
		return err
	}
	for prows.Next() {
		var def string
		if err := prows.Scan(&def); err != nil {
			prows.Close()
			return err
		}
		var d ProjectionDef
		if err := json.Unmarshal([]byte(def), &d); err != nil {
			prows.Close()
			return err
		}
		projections[d.Name] = &d
		for evt := range d.On {
			byEventType[evt] = append(byEventType[evt], &d)
		}
	}
	prows.Close()

	porows, err := r.db.Query(`SELECT def FROM _policies`)
	if err != nil {
		return err
	}
	for porows.Next() {
		var def string
		if err := porows.Scan(&def); err != nil {
			porows.Close()
			return err
		}
		var p PolicyDef
		if err := json.Unmarshal([]byte(def), &p); err != nil {
			porows.Close()
			return err
		}
		policies[p.Name+" "+p.Role] = &p
	}
	porows.Close()

	r.mu.Lock()
	r.projections = projections
	r.byEventType = byEventType
	r.policies = policies
	r.mu.Unlock()
	return nil
}

func (r *Registry) GetProjection(name string) *ProjectionDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projections[name]
}

func (r *Registry) ListProjections() []*ProjectionDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ProjectionDef, 0, len(r.projections))
	for _, d := range r.projections {
		out = append(out, d)
	}
	return out
}

func (r *Registry) DefsForEvent(eventType string) []*ProjectionDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byEventType[eventType]
}

// GetPolicy resolves an exact role match first, then the '*' fallback.
func (r *Registry) GetPolicy(name, role string) *PolicyDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if role != "" {
		if p := r.policies[name+" "+role]; p != nil {
			return p
		}
	}
	return r.policies[name+" *"]
}

// SaveProjection validates, ensures the table, upserts the def, reloads.
func (r *Registry) SaveProjection(d *ProjectionDef) error {
	if d.Columns == nil {
		d.Columns = map[string]string{}
	}
	if d.On == nil {
		d.On = map[string]OpRule{}
	}
	if err := ValidateProjection(d); err != nil {
		return err
	}
	if err := r.ensureTable(d); err != nil {
		return err
	}
	raw, _ := json.Marshal(d)
	_, err := r.db.Exec(
		`INSERT INTO _projections (name, def, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET def = excluded.def, updated_at = excluded.updated_at`,
		d.Name, string(raw), time.Now().UnixMilli(),
	)
	if err != nil {
		return err
	}
	return r.Reload()
}

// SavePolicy validates, upserts, reloads.
func (r *Registry) SavePolicy(p *PolicyDef) error {
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	_, err := r.db.Exec(
		`INSERT INTO _policies (name, role, action, def, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name, role, action) DO UPDATE SET def = excluded.def, updated_at = excluded.updated_at`,
		p.Name, p.Role, p.Action, string(raw), time.Now().UnixMilli(),
	)
	if err != nil {
		return err
	}
	return r.Reload()
}

// ensureTable CREATEs the table (idempotent) then additively ALTERs in any
// declared column missing from the physical table. Every read model has the
// same spine: (tenant, id) PK + updated_at. Identifiers come from the
// validated definition (never client input).
func (r *Registry) ensureTable(def *ProjectionDef) error {
	table := def.TableOf()
	colsSQL := ""
	for name, typ := range def.Columns {
		colsSQL += fmt.Sprintf("%s %s, ", name, r.d.ColumnType(typ))
	}
	create := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
		   tenant TEXT NOT NULL, id TEXT NOT NULL, %s updated_at %s NOT NULL,
		   PRIMARY KEY (tenant, id))`, table, colsSQL, r.d.ColumnType("integer"))
	if _, err := r.db.Exec(create); err != nil {
		return err
	}
	// Additively add any declared column missing from the physical table.
	existing, err := r.d.ExistingColumns(r.db, table)
	if err != nil {
		return err
	}
	for name, typ := range def.Columns {
		if !existing[name] {
			if _, err := r.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, name, r.d.ColumnType(typ))); err != nil {
				return err
			}
		}
	}
	return nil
}
