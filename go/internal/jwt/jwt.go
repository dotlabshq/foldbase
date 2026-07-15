// Package jwt verifies HS256 JWTs, byte-compatible with @baseworks/auth.
package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Claims is the subset the service reads, plus a catch-all.
type Claims map[string]any

// Verify checks an HS256 token against secret and returns its claims, or ok=false
// on any failure (bad shape, wrong signature, expired). Constant-time compare.
func Verify(token, secret string) (Claims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	if exp, ok := claims["exp"].(float64); ok {
		if int64(exp) < time.Now().Unix() {
			return nil, false
		}
	}
	return claims, true
}

// Str returns a string claim, or "" if absent/not a string.
func (c Claims) Str(key string) string {
	if v, ok := c[key].(string); ok {
		return v
	}
	return ""
}
