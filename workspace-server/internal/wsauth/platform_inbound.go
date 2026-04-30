// Package wsauth — platform→workspace inbound secret (per-workspace bearer
// the platform presents when calling INTO a workspace's HTTP server).
//
// Asymmetric to IssueToken in this same package, which mints the
// **outbound** bearer (workspace → platform). See the per-function
// comments and migration 044 for the full rationale on why the two
// roles use distinct secrets stored in different shapes.
//
//	IssueToken                   IssuePlatformInboundSecret
//	──────────                   ───────────────────────────
//	workspace_auth_tokens row    workspaces.platform_inbound_secret column
//	plaintext returned once,     plaintext stored AND returned (the platform
//	  hash stored                  must read it back on every forward call)
//	bcrypt-shape compare          string-equality compare on workspace side
package wsauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
)

// platformInboundSecretBytes is the raw-random length before base64url
// encoding. Same 256-bit entropy floor as workspace_auth_tokens; the
// shape lets a future rotation script substitute one for the other
// without changing the bearer-presenter on either side.
const platformInboundSecretBytes = 32

// ErrNoInboundSecret is returned by ReadPlatformInboundSecret when the
// row exists but the column is NULL or empty. Callers MUST treat this
// as a structural failure (the row was created by a path that didn't
// mint a secret, or the migration ran but the row predates it) and
// surface the nil bearer to the user as a 500-class error rather than
// silently sending an unauthenticated request to the workspace.
var ErrNoInboundSecret = errors.New("wsauth: workspace has no platform_inbound_secret on file")

// IssuePlatformInboundSecret generates a fresh per-workspace shared
// secret, persists the plaintext into workspaces.platform_inbound_secret,
// and returns the plaintext so the provisioner can write it into
// /configs/.platform_inbound_secret on the workspace's volume.
//
// The plaintext is INTENTIONALLY retained on the platform side: every
// platform→workspace forward call reads it back to put in the
// Authorization header. Hashing would force a re-mint on every call
// (defeating the purpose of the shared secret) or a separate plaintext
// store (defeating the simplicity). Encryption-at-rest is delegated to
// the underlying Postgres volume — application-layer encryption via
// SECRETS_ENCRYPTION_KEY is a defense-in-depth follow-up.
func IssuePlatformInboundSecret(ctx context.Context, db *sql.DB, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", fmt.Errorf("wsauth: workspaceID required")
	}
	buf := make([]byte, platformInboundSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("wsauth: generate platform_inbound_secret: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)

	_, err := db.ExecContext(ctx, `
		UPDATE workspaces SET platform_inbound_secret = $1 WHERE id = $2
	`, plaintext, workspaceID)
	if err != nil {
		return "", fmt.Errorf("wsauth: persist platform_inbound_secret: %w", err)
	}
	return plaintext, nil
}

// ReadPlatformInboundSecret returns the plaintext secret for a workspace
// or ErrNoInboundSecret if the column is NULL/empty. Used by platform-
// side handlers that forward HTTPS calls into the workspace.
//
// Callers MUST handle ErrNoInboundSecret explicitly; never default to
// an empty bearer. An empty Authorization header would let any caller
// through if the workspace's auth is fail-open (it is not today, but
// defense-in-depth keeps it that way).
func ReadPlatformInboundSecret(ctx context.Context, db *sql.DB, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", fmt.Errorf("wsauth: workspaceID required")
	}
	var secret sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT platform_inbound_secret FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&secret)
	if err == sql.ErrNoRows {
		return "", ErrNoInboundSecret
	}
	if err != nil {
		return "", fmt.Errorf("wsauth: read platform_inbound_secret: %w", err)
	}
	if !secret.Valid || secret.String == "" {
		return "", ErrNoInboundSecret
	}
	return secret.String, nil
}
