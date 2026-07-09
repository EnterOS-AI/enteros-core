package boottoken

import (
	"crypto/subtle"
	"errors"
	"os"
	"time"
)

var (
	// ErrNoKey means the platform has no ADMIN_TOKEN to verify against (the
	// per-tenant HMAC key). Fail closed — the route must 401/503, never accept.
	ErrNoKey = errors.New("boottoken: no ADMIN_TOKEN key available")
	// ErrScope means the token is valid but lacks the required scope.
	ErrScope = errors.New("boottoken: missing required scope")
	// ErrWorkspaceMismatch means the token is bound to a different workspace than
	// the one being restored.
	ErrWorkspaceMismatch = errors.New("boottoken: workspace mismatch")
)

// VerifyRestore validates a boot token presented to the object-store
// restore-on-boot route (GET /workspaces/:id/restore). It is the pre-register
// counterpart of middleware.WorkspaceAuth: the box has no per-workspace token yet
// (restore runs in cloud-init, before the container registers), so it presents
// this CP-minted scoped token instead.
//
// The HMAC key is the tenant platform's own ADMIN_TOKEN — the same per-tenant
// admin_token the CP minted the token against — so verification is offline (no CP
// round-trip). The token must:
//   - verify + be unexpired (Verify),
//   - carry ScopeRestore (never a token minted only for boot-events),
//   - be bound to wsID (the :id path param), so a token for workspace A cannot
//     restore workspace B.
//
// keyOverride is for tests; production passes "" to read ADMIN_TOKEN from the env.
func VerifyRestore(token, wsID string, now time.Time, keyOverride string) (Claims, error) {
	key := keyOverride
	if key == "" {
		key = os.Getenv("ADMIN_TOKEN")
	}
	if key == "" {
		return Claims{}, ErrNoKey
	}
	c, err := Verify(token, []byte(key), now)
	if err != nil {
		return Claims{}, err
	}
	if !c.HasScope(ScopeRestore) {
		return Claims{}, ErrScope
	}
	if wsID == "" || subtle.ConstantTimeCompare([]byte(c.WorkspaceID), []byte(wsID)) != 1 {
		return Claims{}, ErrWorkspaceMismatch
	}
	return c, nil
}
