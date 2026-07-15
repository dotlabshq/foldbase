package readmodel

import (
	"fmt"
	"sort"
	"strings"
)

const (
	defaultLimit = 1000
	maxLimit     = 10000
)

var ops = map[string]string{
	"eq": "=", "ne": "!=", "gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "like": "LIKE",
}

// QueryResult is the query response.
type QueryResult struct {
	Rows   []map[string]any `json:"rows"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// allowedColumns computes the whitelist: id + declared columns + updated_at,
// filtered by the policy's allow/deny.
func allowedColumns(def *ProjectionDef, policy *PolicyDef) map[string]bool {
	cols := map[string]bool{"id": true, "updated_at": true}
	for c := range def.Columns {
		cols[c] = true
	}
	if len(policy.Allow) > 0 {
		allow := map[string]bool{"id": true}
		for _, c := range policy.Allow {
			allow[c] = true
		}
		for c := range cols {
			if !allow[c] {
				delete(cols, c)
			}
		}
	}
	for _, c := range policy.Deny {
		delete(cols, c)
	}
	return cols
}

// compileUsing binds the operator-authored `using` fragment's :auth_* holders.
func compileUsing(using string, ctx AuthCtx) (string, []any, error) {
	if strings.Contains(using, ";") {
		return "", nil, &ValidationError{"policy using expression must be a single fragment"}
	}
	var args []any
	var outErr error
	out := replaceAuth(using, func(name string) (any, bool) {
		key := name[len(":auth_"):]
		var value any
		switch {
		case key == "uid":
			if ctx.UID == "" {
				return nil, false
			}
			value = ctx.UID
		case key == "role":
			if ctx.Role == "" {
				return nil, false
			}
			value = ctx.Role
		case key == "email":
			if ctx.Email == "" {
				return nil, false
			}
			value = ctx.Email
		case key == "tenant":
			value = ctx.Tenant
		case strings.HasPrefix(key, "claim_"):
			v, ok := ctx.Claims[key[len("claim_"):]]
			if !ok || v == nil {
				return nil, false
			}
			value = v
		default:
			return nil, false
		}
		args = append(args, value)
		return value, true
	}, func(missing string) {
		if outErr == nil {
			outErr = &ForbiddenError{"policy requires " + missing + " but it is not present in the auth context"}
		}
	})
	if outErr != nil {
		return "", nil, outErr
	}
	return out, args, nil
}

// replaceAuth replaces every :auth_<key> token; onMissing fires when the
// resolver rejects one (→ deny). Returns the rewritten fragment with '?'.
func replaceAuth(s string, resolve func(string) (any, bool), onMissing func(string)) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], ":auth_") {
			j := i + len(":auth_")
			for j < len(s) && (s[j] == '_' || (s[j] >= 'a' && s[j] <= 'z')) {
				j++
			}
			name := s[i:j]
			if _, ok := resolve(name); ok {
				b.WriteByte('?')
			} else {
				onMissing(name)
				b.WriteString(name)
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// compileWhere compiles the client's where tree into parameterized SQL over
// whitelisted columns.
func compileWhere(node any, cols map[string]bool) (string, []any, error) {
	m, ok := node.(map[string]any)
	if !ok {
		return "1=1", nil, nil
	}
	if andRaw, ok := m["and"]; ok {
		return compileGroup(andRaw, cols, "AND", "1=1")
	}
	if orRaw, ok := m["or"]; ok {
		return compileGroup(orRaw, cols, "OR", "1=0")
	}

	// leaf: column → {op: value}
	var clauses []string
	var args []any
	// deterministic column order
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, col := range keys {
		if !cols[col] {
			return "", nil, &ValidationError{"unknown or forbidden column '" + col + "'"}
		}
		opMap, ok := m[col].(map[string]any)
		if !ok {
			return "", nil, &ValidationError{"filter for '" + col + "' must be an operator map"}
		}
		opKeys := make([]string, 0, len(opMap))
		for k := range opMap {
			opKeys = append(opKeys, k)
		}
		sort.Strings(opKeys)
		for _, op := range opKeys {
			value := opMap[op]
			if value == nil && op != "eq" && op != "ne" {
				continue
			}
			switch op {
			case "in":
				list, ok := value.([]any)
				if !ok {
					return "", nil, &ValidationError{"'in' requires an array"}
				}
				if len(list) == 0 {
					clauses = append(clauses, "1=0")
					continue
				}
				ph := strings.TrimSuffix(strings.Repeat("?, ", len(list)), ", ")
				clauses = append(clauses, fmt.Sprintf("%s IN (%s)", col, ph))
				args = append(args, list...)
			case "eq":
				if value == nil {
					clauses = append(clauses, col+" IS NULL")
				} else {
					clauses = append(clauses, col+" = ?")
					args = append(args, normalize(value))
				}
			case "ne":
				if value == nil {
					clauses = append(clauses, col+" IS NOT NULL")
				} else {
					clauses = append(clauses, col+" != ?")
					args = append(args, normalize(value))
				}
			default:
				sqlOp, ok := ops[op]
				if !ok {
					return "", nil, &ValidationError{"unknown operator '" + op + "'"}
				}
				clauses = append(clauses, fmt.Sprintf("%s %s ?", col, sqlOp))
				args = append(args, normalize(value))
			}
		}
	}
	if len(clauses) == 0 {
		return "1=1", nil, nil
	}
	return "(" + strings.Join(clauses, " AND ") + ")", args, nil
}

func compileGroup(raw any, cols map[string]bool, joiner, empty string) (string, []any, error) {
	list, ok := raw.([]any)
	if !ok {
		return "", nil, &ValidationError{"and/or requires an array"}
	}
	if len(list) == 0 {
		return empty, nil, nil
	}
	var parts []string
	var args []any
	for _, n := range list {
		s, a, err := compileWhere(n, cols)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, s)
		args = append(args, a...)
	}
	return "(" + strings.Join(parts, " "+joiner+" ") + ")", args, nil
}

// normalize coerces booleans to 0/1 (SQLite has no bool).
func normalize(v any) any {
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	return v
}

// ExecQuery runs a query request against a registered projection, enforcing the
// caller's policy. Returns typed errors the host maps to 404/403/400.
func ExecQuery(db SQLDB, reg *Registry, name string, request map[string]any, ctx AuthCtx) (*QueryResult, error) {
	def := reg.GetProjection(name)
	if def == nil {
		return nil, &NotFoundError{"no projection named '" + name + "'"}
	}
	policy := reg.GetPolicy(name, ctx.Role)
	if policy == nil {
		role := ctx.Role
		if role == "" {
			role = "*"
		}
		return nil, &ForbiddenError{"no select policy for '" + name + "' and role '" + role + "'"}
	}

	cols := allowedColumns(def, policy)

	// SELECT list
	var selectCols []string
	if raw, ok := request["select"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, &ValidationError{"select must be an array"}
		}
		for _, c := range list {
			s, ok := c.(string)
			if !ok || !isIdentifier(s) || !cols[s] {
				return nil, &ValidationError{fmt.Sprintf("unknown or forbidden column '%v'", c)}
			}
			selectCols = append(selectCols, s)
		}
	}
	if len(selectCols) == 0 {
		for c := range cols {
			selectCols = append(selectCols, c)
		}
		sort.Strings(selectCols)
	}

	where := []string{"tenant = ?"}
	args := []any{ctx.Tenant}

	if policy.Using != "" {
		frag, uArgs, err := compileUsing(policy.Using, ctx)
		if err != nil {
			return nil, err
		}
		where = append(where, "("+frag+")")
		args = append(args, uArgs...)
	}

	if raw, ok := request["where"]; ok && raw != nil {
		frag, wArgs, err := compileWhere(raw, cols)
		if err != nil {
			return nil, err
		}
		where = append(where, frag)
		args = append(args, wArgs...)
	}

	// ORDER BY
	var order []string
	if raw, ok := request["sort"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, &ValidationError{"sort must be an array"}
		}
		for _, s := range list {
			entry, ok := s.(string)
			if !ok || !sortRe.MatchString(entry) {
				return nil, &ValidationError{fmt.Sprintf("invalid sort '%v'", s)}
			}
			desc := strings.HasPrefix(entry, "-")
			col := entry
			if desc {
				col = entry[1:]
			}
			if !cols[col] {
				return nil, &ValidationError{"unknown or forbidden sort column '" + col + "'"}
			}
			if desc {
				order = append(order, col+" DESC")
			} else {
				order = append(order, col+" ASC")
			}
		}
	}

	limit := defaultLimit
	if raw, ok := request["limit"]; ok {
		if f, ok := raw.(float64); ok && f > 0 {
			limit = int(f)
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := 0
	if raw, ok := request["offset"]; ok {
		if f, ok := raw.(float64); ok && f >= 0 {
			offset = int(f)
		}
	}

	sqlStr := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(selectCols, ", "), def.TableOf(), strings.Join(where, " AND "))
	if len(order) > 0 {
		sqlStr += " ORDER BY " + strings.Join(order, ", ")
	}
	sqlStr += " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	colNames, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range colNames {
			row[c] = coerce(vals[i])
		}
		out = append(out, row)
	}
	return &QueryResult{Rows: out, Limit: limit, Offset: offset}, rows.Err()
}

// coerce turns driver []byte into string for clean JSON output.
func coerce(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
