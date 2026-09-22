package readmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// FoldError is a rule that cannot be applied to a given event — a path the
// payload does not carry, a value of the wrong type. It is deterministic:
// replaying the same event against the same definition fails the same way, so
// a rebuild counts it and carries on rather than abandoning the whole view.
//
// An SQL failure is deliberately NOT a FoldError. That one is transient and
// must abort, or a rebuild would report success over a database that was
// refusing every write.
type FoldError struct{ Msg string }

func (e *FoldError) Error() string { return e.Msg }

// payloadPath resolves a "$.a.b" path against the event payload. The second
// result reports whether the path exists at all: a field that is absent and a
// field explicitly set to null are different facts, and inc has to tell them
// apart.
func payloadPath(payload map[string]any, path string) (any, bool) {
	var cur any = payload
	for _, key := range strings.Split(path[2:], ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// resolveSet: a string starting with "$." is a payload path, else a literal.
// The resolved value is coerced to something SQLite can bind: booleans → 0/1,
// arrays/objects → JSON text (the server-side counterpart of jsonCol).
func resolveSet(payload map[string]any, value any) any {
	var resolved any
	if s, ok := value.(string); ok && strings.HasPrefix(s, "$.") {
		// A path that does not resolve writes NULL, as it always has: for a
		// set column the absence is visible to whoever reads the row.
		resolved, _ = payloadPath(payload, s)
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

// resolveInc resolves one inc value to the number to add. A literal is used
// as-is; a "$." path is resolved against the payload, where three outcomes are
// deliberately different:
//
//   - the field is absent      → error. The definition names a field the event
//     does not carry, which is a bug and has to be loud. A silent 0 is
//     indistinguishable from a real total of zero, so nothing would ever look
//     wrong and nobody would run the rebuild that could heal it.
//   - the field is null        → 0. The app said so deliberately.
//   - the field is not numeric → error. A type mismatch is always a bug.
//
// An error here never loses the event. The fold runs after the commit, so the
// append still succeeds with projected:false and a rebuild recovers the true
// total from the log — the stale-view failure iron rule 1 describes.
//
// This is why inc is stricter than set: NULL announces itself to a reader,
// a 0 in a total does not.
func resolveInc(payload map[string]any, column string, value any) (float64, error) {
	resolved := value
	if s, ok := value.(string); ok && strings.HasPrefix(s, "$.") {
		v, found := payloadPath(payload, s)
		if !found {
			return 0, &FoldError{fmt.Sprintf("inc %q: the payload has no %s", column, s)}
		}
		if v == nil {
			return 0, nil
		}
		resolved = v
	}
	switch n := resolved.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, &FoldError{fmt.Sprintf("inc %q: %#v is not a number", column, resolved)}
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
	// Resolve every inc before touching the database: a rule that cannot be
	// resolved must leave no row behind, not a half-applied one.
	incArgs := []any{}
	for _, c := range incCols {
		n, err := resolveInc(e.Payload, c, rule.Inc[c])
		if err != nil {
			return err
		}
		args = append(args, n)
		incArgs = append(incArgs, n)
	}
	args = append(args, time.Now().UnixMilli())

	updates := []string{}
	for _, c := range setCols {
		updates = append(updates, fmt.Sprintf("%s = excluded.%s", c, c))
	}
	for _, c := range incCols {
		// Qualify the existing value with the table: in Postgres a bare column in
		// DO UPDATE is ambiguous between the target row and `excluded`. SQLite
		// accepts the qualified form too.
		updates = append(updates, fmt.Sprintf("%s = COALESCE(%s.%s, 0) + ?", c, table, c))
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
// events (must be fed in globalSeq order). Returns how many events the
// definition could not fold.
//
// Those are skipped rather than fatal. A rebuild is the repair mechanism iron
// rule 1 promises — "a fold failure can only leave a view stale, repaired by
// replaying the log" — so one event the current rules cannot fold must not take
// the repair away from every event that can be. The count is reported back so
// the gap is visible instead of guessed at.
func RebuildProjection(db SQLDB, reg *Registry, name, tenant string, events []EventLike) (int, error) {
	def := reg.GetProjection(name)
	if def == nil {
		return 0, nil
	}
	if _, err := db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE tenant = ?`, def.TableOf()), tenant); err != nil {
		return 0, err
	}
	skipped := 0
	for _, e := range events {
		if e.Tenant != tenant {
			continue
		}
		if err := applyTo(db, def, e); err != nil {
			var fe *FoldError
			if errors.As(err, &fe) {
				skipped++
				log.Printf("[foldbase] rebuild %s: skipping %s on %s: %v", name, e.Type, e.StreamID, err)
				continue
			}
			return skipped, err
		}
	}
	return skipped, nil
}

// RebuildTenant rebuilds every registered projection for a tenant, returning
// the total number of events no definition could fold.
func RebuildTenant(db SQLDB, reg *Registry, tenant string, events []EventLike) (int, error) {
	skipped := 0
	for _, def := range reg.ListProjections() {
		n, err := RebuildProjection(db, reg, def.Name, tenant, events)
		skipped += n
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}
