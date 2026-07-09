// Package boottoken verifies (and, for tests, mints) the WS-A scoped workspace
// boot token.
//
// A workspace box authenticates its PRE-register operations — the object-store
// platform-proxy restore-on-boot and its boot-event phone-home — with this token,
// because it does NOT hold a per-workspace token yet (that is minted at
// /registry/register, after the container starts) and, per the founder ruling
// (2026-07-08), must NOT carry the tenant admin_token. molecule-controlplane MINTS
// the token at provision; this package is the molecule-core VERIFIER (the tenant
// platform validates it on the restore route).
//
// The wire format is the SSOT contract molecule-ai-sdk contracts/boot-token/
// (schema + golden test vectors). This file MUST reproduce that construction
// byte-for-byte — token_test.go pins it against the published golden vector, so a
// drift from the controlplane minter fails CI here.
//
//	token = base64url(payloadJSON) "." base64url(HMAC-SHA256(key, base64url(payloadJSON)))
//
// The HMAC key is the per-tenant admin_token (the platform box holds it in
// ADMIN_TOKEN); it is the signing key ONLY, never transmitted, so the token
// conveys no admin power.
package boottoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// ScopeBootEvent authorizes POST /cp/tenants/boot-event (verified by the CP).
	ScopeBootEvent = "boot-event"
	// ScopeRestore authorizes the object-store platform-proxy restore-on-boot route.
	ScopeRestore = "restore"
)

var (
	ErrMalformed = errors.New("boottoken: malformed token")
	ErrBadSig    = errors.New("boottoken: signature mismatch")
	ErrExpired   = errors.New("boottoken: expired")
)

// Claims is the signed payload. Field order + JSON keys are the wire shape pinned
// by the SDK boot-token contract — do not reorder or rename without bumping the
// contract (it would break every in-flight token).
type Claims struct {
	WorkspaceID string   `json:"wsid"`
	OrgID       string   `json:"org"`
	Expiry      int64    `json:"exp"`
	Scopes      []string `json:"scope"`
}

// HasScope reports whether s is granted.
func (c Claims) HasScope(s string) bool {
	for _, x := range c.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// Mint signs claims with key. Present for test symmetry with the controlplane
// minter (the golden-vector test asserts this reproduces the published token);
// production molecule-core only verifies.
func Mint(key []byte, c Claims) (string, error) {
	if len(key) == 0 {
		return "", ErrBadSig
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + sign(key, p), nil
}

func sign(key []byte, p string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// LooksLike is a cheap structural discriminator: exactly one dot, non-empty sides.
func LooksLike(token string) bool {
	p, sig, ok := split(token)
	return ok && p != "" && sig != ""
}

func split(token string) (payload, sig string, ok bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i >= len(token)-1 {
		return "", "", false
	}
	if strings.IndexByte(token[i+1:], '.') >= 0 {
		return "", "", false
	}
	return token[:i], token[i+1:], true
}

// ParseUnverified decodes the payload WITHOUT checking the signature. The claim
// is untrusted until Verify passes.
func ParseUnverified(token string) (Claims, error) {
	p, _, ok := split(token)
	if !ok {
		return Claims{}, ErrMalformed
	}
	return decode(p)
}

func decode(p string) (Claims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	return c, nil
}

// Verify checks the HMAC (constant-time) against key and the expiry against now.
// Route-specific scope + workspace binding are the caller's (see VerifyRestore).
func Verify(token string, key []byte, now time.Time) (Claims, error) {
	p, sig, ok := split(token)
	if !ok {
		return Claims{}, ErrMalformed
	}
	if len(key) == 0 {
		subtle.ConstantTimeCompare([]byte(sig), []byte(sign([]byte("\x00"), p)))
		return Claims{}, ErrBadSig
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(sign(key, p))) != 1 {
		return Claims{}, ErrBadSig
	}
	c, err := decode(p)
	if err != nil {
		return Claims{}, err
	}
	if now.Unix() > c.Expiry {
		return Claims{}, ErrExpired
	}
	return c, nil
}
