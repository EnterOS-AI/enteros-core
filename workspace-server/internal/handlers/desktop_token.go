package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// DeriveDesktopControlToken deterministically derives the per-sidecar control-
// server bearer for a workspace (design §6.5). Both the provisioner (which sets
// DESKTOP_CONTROL_TOKEN on the sidecar) and the platform gateway's TokenResolver
// derive the SAME value from the shared signing secret, so no per-sidecar secret
// needs to be stored anywhere. Empty when no secret is configured (fail-closed).
func DeriveDesktopControlToken(secret, workspaceID string) string {
	if secret == "" || workspaceID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("desktop-control:" + workspaceID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
