// Package auth implements the service trust boundary (ADR-002/003/004/005):
// declared auth modes, token-type capabilities, and service-token tenant
// selection. Mirrors src/lib/auth.ts of the TS reference exactly.
package auth

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/dotlabshq/foldbase/internal/jwt"
)

type Mode string

const (
	ModeNone       Mode = "none"
	ModeServiceJWT Mode = "service-jwt"
	ModeUserJWT    Mode = "user-jwt"
)

func env(key string) string {
	return os.Getenv("FOLDBASE_" + key)
}

func secret() string {
	if s := env("JWT_SECRET"); s != "" {
		return s
	}
	return os.Getenv("JWT_SECRET")
}

// CurrentMode resolves the declared mode, falling back to legacy inference for
// dev convenience (a secret implies verified tokens).
func CurrentMode() Mode {
	switch env("AUTH") {
	case "none":
		return ModeNone
	case "service-jwt":
		return ModeServiceJWT
	case "user-jwt":
		return ModeUserJWT
	}
	if secret() != "" {
		return ModeServiceJWT
	}
	return ModeNone
}

// AssertConfig fails closed on misconfiguration (ADR-002).
func AssertConfig() error {
	m := env("AUTH")
	if (m == "service-jwt" || m == "user-jwt") && secret() == "" {
		return errors.New("FOLDBASE_AUTH=" + m + " requires FOLDBASE_JWT_SECRET")
	}
	if os.Getenv("NODE_ENV") == "production" && m == "" {
		return errors.New(`production requires an explicit FOLDBASE_AUTH (set "none" to run open)`)
	}
	return nil
}

// Ctx is the end-user identity bound into policy :auth_* placeholders.
type Ctx struct {
	UID   string
	Role  string
	Email string
}

// Resolved is the outcome of a successful auth resolution.
type Resolved struct {
	Tenant     string
	Ctx        Ctx
	CanWrite   bool // may append
	CanControl bool // may use the control plane
	Subject    string
}

// Error carries an HTTP status for a failed resolution.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

func headerCtx(r *http.Request) Ctx {
	return Ctx{
		UID:   r.Header.Get("X-Auth-UID"),
		Role:  r.Header.Get("X-Auth-Role"),
		Email: r.Header.Get("X-Auth-Email"),
	}
}

func adminOK(r *http.Request) bool {
	admin := env("ADMIN_TOKEN")
	if admin == "" {
		return true
	}
	return bearer(r) == admin
}

// Resolve enforces the mode/token rules and returns the caller's tenant +
// capabilities, or an *Error with an HTTP status.
func Resolve(r *http.Request) (*Resolved, *Error) {
	mode := CurrentMode()

	if mode == ModeNone {
		tenant := r.Header.Get("X-Tenant-ID")
		if tenant == "" {
			return nil, &Error{400, "X-Tenant-ID header is required"}
		}
		return &Resolved{Tenant: tenant, Ctx: headerCtx(r), CanWrite: true, CanControl: adminOK(r)}, nil
	}

	s := secret()
	if s == "" {
		return nil, &Error{401, "server misconfigured: auth enabled without secret"}
	}
	token := bearer(r)
	if token == "" {
		return nil, &Error{401, "missing bearer token"}
	}
	claims, ok := jwt.Verify(token, s)
	if !ok {
		return nil, &Error{401, "invalid or expired token"}
	}

	isService := claims.Str("type") == "service"

	if !isService {
		if mode == ModeServiceJWT {
			return nil, &Error{401, "user tokens are not accepted in service-jwt mode"}
		}
		tenant := claims.Str("org_id")
		if tenant == "" {
			return nil, &Error{401, "invalid or unscoped token"}
		}
		sub := claims.Str("sub")
		return &Resolved{
			Tenant:   tenant,
			Ctx:      Ctx{UID: sub, Role: claims.Str("role"), Email: claims.Str("email")},
			CanWrite: false, CanControl: false, Subject: sub,
		}, nil
	}

	// service token — tenant selection (ADR-004)
	pin := claims.Str("org_id")
	header := r.Header.Get("X-Tenant-ID")
	var tenant string
	if pin != "" {
		if header != "" && header != pin {
			return nil, &Error{403, "X-Tenant-ID conflicts with the token org_id"}
		}
		tenant = pin
	} else {
		if header == "" {
			return nil, &Error{400, "X-Tenant-ID header is required for service tokens"}
		}
		tenant = header
	}
	return &Resolved{
		Tenant: tenant, Ctx: headerCtx(r),
		CanWrite: true, CanControl: true, Subject: claims.Str("sub"),
	}, nil
}
