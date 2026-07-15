package readmodel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// payloadPath resolves a "$.a.b" path against the event payload; missing → nil.
func payloadPath(payload map[string]any, path string) any {
	var cur any = payload
	for _, key := range strings.Split(path[2:], ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// resolveSet: a string starting with "$." is a payload path, else a literal.
// The resolved value is coerced to something SQLite can bind: booleans → 0/1,
// arrays/objects → JSON text (the server-side counterpart of jsonCol).
func resolveSet(payload map[string]any, value any) any {
	var resolved any
	if s, ok := value.(string); ok && strings.HasPrefix(s, "$.") {
		resolved = payloadPath(payload, s)
	} else {
		resolved = value
	}
	switch v := resolved.(type) {
	case nil:
		return nil
	case bool:
		if v {
			return 1
		}
		return 0
	case string, float64, int, int64:
		return resolved
	default:
		b, _ := json.Marshal(resolved)
		return string(b)
	}
}

// applyTo applies one event to one projection.
func applyTo(db SQLDB, def *ProjectionDef, e EventLike) error {
	rule, ok := def.On[e.Type]
	if !ok {
		return nil
	}
	table := def.TableOf()

	if rule.Op == "delete" {
		_, err := db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE tenant = ? AND id = ?`, table), e.Tenant, e.StreamID)
		return err
	}

	// upsert — insert the row, or merge ONLY this rule's columns into it.
	var setCols, incCols []string
	for c := range rule.Set {
		if _, declared := def.Columns[c]; declared {
			setCols = append(setCols, c)
		}
	}
	for c := range rule.Inc {
		if _, declared := def.Columns[c]; declared {
			incCols = append(incCols, c)
		}
	}

	insertCols := append([]string{"tenant", "id"}, append(append([]string{}, setCols...), incCols...)...)
	insertCols = append(insertCols, "updated_at")

	args := []any{e.Tenant, e.StreamID}
	for _, c := range setCols {
		args = append(args, resolveSet(e.Payload, rule.Set[c]))
	}
	incArgs := []any{}
	for _, c := range incCols {
		args = append(args, rule.Inc[c])
		incArgs = append(incArgs, rule.Inc[c])
	}
	args = append(args, time.Now().UnixMilli())

	updates := []string{}
	for _, c := range setCols {
		updates = append(updates, fmt.Sprintf("%s = excluded.%s", c, c))
	}
	for _, c := range incCols {
		updates = append(updates, fmt.Sprintf("%s = COALESCE(%s, 0) + ?", c, c))
	}
	updates = append(updates, "updated_at = excluded.updated_at")

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(insertCols)), ", ")
	sqlStr := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(tenant, id) DO UPDATE SET %s`,
		table, strings.Join(insertCols, ", "), placeholders, strings.Join(updates, ", "),
	)
	// DO UPDATE's inc placeholders bind after the INSERT args (same order as TS).
	args = append(args, incArgs...)
	_, err := db.Exec(sqlStr, args...)
	return err
}

// ApplyEvent folds one event into every projection with a rule for its type.
func ApplyEvent(db SQLDB, reg *Registry, e EventLike) error {
	for _, def := range reg.DefsForEvent(e.Type) {
		if err := applyTo(db, def, e); err != nil {
			return err
		}
	}
	return nil
}

// RebuildProjection wipes one projection for one tenant, then refolds the given
// events (must be fed in globalSeq order).
func RebuildProjection(db SQLDB, reg *Registry, name, tenant string, events []EventLike) error {
	def := reg.GetProjection(name)
	if def == nil {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE tenant = ?`, def.TableOf()), tenant); err != nil {
		return err
	}
	for _, e := range events {
		if e.Tenant != tenant {
			continue
		}
		if err := applyTo(db, def, e); err != nil {
			return err
		}
	}
	return nil
}

// RebuildTenant rebuilds every registered projection for a tenant.
func RebuildTenant(db SQLDB, reg *Registry, tenant string, events []EventLike) error {
	for _, def := range reg.ListProjections() {
		if err := RebuildProjection(db, reg, def.Name, tenant, events); err != nil {
			return err
		}
	}
	return nil
}
