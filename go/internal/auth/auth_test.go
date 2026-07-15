package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

const secretVal = "test-secret-32-chars-minimum-okey!"

func sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	claims["iat"] = time.Now().Unix()
	claims["exp"] = time.Now().Unix() + 3600
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	h := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pj, _ := json.Marshal(claims)
	p := b64(pj)
	mac := hmac.New(sha256.New, []byte(secretVal))
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + b64(mac.Sum(nil))
}

func TestNoneModeRequiresTenantHeader(t *testing.T) {
	t.Setenv("FOLDBASE_AUTH", "none")
	r := httptest.NewRequest("GET", "/v1/events", nil)
	if _, err := Resolve(r); err == nil || err.Status != 400 {
		t.Fatalf("expected 400, got %v", err)
	}
	r.Header.Set("X-Tenant-ID", "acme")
	res, err := Resolve(r)
	if err != nil || res.Tenant != "acme" || !res.CanControl {
		t.Fatalf("none mode resolution wrong: %+v %v", res, err)
	}
}

func TestServiceJwtRejectsUserTokens(t *testing.T) {
	t.Setenv("FOLDBASE_AUTH", "service-jwt")
	t.Setenv("FOLDBASE_JWT_SECRET", secretVal)
	r := httptest.NewRequest("GET", "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+sign(t, map[string]any{"sub": "u1", "type": "user", "org_id": "acme"}))
	if _, err := Resolve(r); err == nil || err.Status != 401 {
		t.Fatalf("user token should be rejected in service-jwt mode, got %v", err)
	}
}

func TestServiceTokenTenantSelectionAndPinning(t *testing.T) {
	t.Setenv("FOLDBASE_AUTH", "service-jwt")
	t.Setenv("FOLDBASE_JWT_SECRET", secretVal)

	// no X-Tenant-ID → 400 (no default tenant, ADR-004)
	r := httptest.NewRequest("GET", "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+sign(t, map[string]any{"sub": "app", "type": "service"}))
	if _, err := Resolve(r); err == nil || err.Status != 400 {
		t.Fatalf("expected 400 without tenant header, got %v", err)
	}

	// pinned org_id + conflicting header → 403
	r2 := httptest.NewRequest("GET", "/v1/events", nil)
	r2.Header.Set("Authorization", "Bearer "+sign(t, map[string]any{"sub": "app", "type": "service", "org_id": "acme"}))
	r2.Header.Set("X-Tenant-ID", "evil")
	if _, err := Resolve(r2); err == nil || err.Status != 403 {
		t.Fatalf("expected 403 on pin mismatch, got %v", err)
	}
}

func TestUserJwtIdentityFromClaimsHeadersIgnored(t *testing.T) {
	t.Setenv("FOLDBASE_AUTH", "user-jwt")
	t.Setenv("FOLDBASE_JWT_SECRET", secretVal)
	r := httptest.NewRequest("GET", "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+sign(t, map[string]any{"sub": "u1", "type": "user", "org_id": "acme", "role": "member"}))
	r.Header.Set("X-Auth-UID", "spoofed")
	r.Header.Set("X-Tenant-ID", "spoofed")
	res, err := Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tenant != "acme" || res.Ctx.UID != "u1" || res.CanWrite || res.CanControl {
		t.Fatalf("user token capabilities wrong: %+v", res)
	}
}

func TestAssertConfigFailsClosed(t *testing.T) {
	t.Setenv("FOLDBASE_AUTH", "service-jwt")
	t.Setenv("FOLDBASE_JWT_SECRET", "")
	t.Setenv("JWT_SECRET", "")
	if err := AssertConfig(); err == nil {
		t.Fatal("expected boot refusal without secret")
	}
	t.Setenv("FOLDBASE_AUTH", "")
	t.Setenv("NODE_ENV", "production")
	if err := AssertConfig(); err == nil {
		t.Fatal("expected boot refusal in production without explicit mode")
	}
}
