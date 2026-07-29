package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

const (
	displayControlDefaultTTLSeconds = 300
	displayControlMinTTLSeconds     = 30
	displayControlMaxTTLSeconds     = 3600
)

// acquireDisplayControlQuery upserts the display-control lock. The WHERE clause
// on the UPDATE is the arbitration policy (see AcquireDisplayControl for the
// human-preempts-agent semantics); it is a package const so the real-PG
// integration test exercises the EXACT statement the handler runs (no drift).
const acquireDisplayControlQuery = `
INSERT INTO workspace_display_control_locks
    (workspace_id, controller, controlled_by, expires_at)
VALUES
    ($1, $2, $3, now() + ($4 * interval '1 second'))
ON CONFLICT (workspace_id) DO UPDATE
SET controller = EXCLUDED.controller,
    controlled_by = EXCLUDED.controlled_by,
    expires_at = EXCLUDED.expires_at,
    updated_at = now()
WHERE workspace_display_control_locks.expires_at <= now()
   OR workspace_display_control_locks.controlled_by = EXCLUDED.controlled_by
   OR (workspace_display_control_locks.controller = 'agent' AND EXCLUDED.controller = 'user')
RETURNING controller, controlled_by, expires_at`

type workspaceDisplayControlResponse struct {
	Controller     string    `json:"controller"`
	ControlledBy   string    `json:"controlled_by,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	SessionURL     string    `json:"session_url,omitempty"`
	ViewSessionURL string    `json:"view_session_url,omitempty"`
}

type workspaceDisplayControlNoneResponse struct {
	Controller     string `json:"controller"`
	ViewSessionURL string `json:"view_session_url,omitempty"`
}

type acquireDisplayControlRequest struct {
	Controller string `json:"controller"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type releaseDisplayControlRequest struct {
	Force bool `json:"force"`
}

// DisplayControl handles GET /workspaces/:id/display/control.
func (h *WorkspaceHandler) DisplayControl(c *gin.Context) {
	lock, found, err := h.loadActiveDisplayControl(c, c.Param("id"))
	if err != nil {
		log.Printf("DisplayControl: load lock for %s failed: %v", c.Param("id"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
		return
	}
	// Issue a view-only URL either way (§8): watching never requires holding
	// control, so any authorized caller polling the lock state also gets a URL to
	// watch. Empty when signing is unconfigured.
	viewURL := signedDisplayViewerURL(c.Param("id"))
	if !found {
		c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none", ViewSessionURL: viewURL})
		return
	}
	lock.ViewSessionURL = viewURL
	c.JSON(http.StatusOK, lock)
}

// AcquireDisplayControl handles POST /workspaces/:id/display/control/acquire.
func (h *WorkspaceHandler) AcquireDisplayControl(c *gin.Context) {
	var req acquireDisplayControlRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid display control request"})
			return
		}
	}
	if req.Controller == "" {
		req.Controller = "user"
	}
	if req.Controller != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "browser callers may only acquire user display control"})
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = displayControlDefaultTTLSeconds
	}
	if req.TTLSeconds < displayControlMinTTLSeconds || req.TTLSeconds > displayControlMaxTTLSeconds {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ttl_seconds must be between 30 and 3600"})
		return
	}
	if ok := h.displayControlEnabled(c, c.Param("id")); !ok {
		return
	}

	controlledBy, ok := displayControlActor(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control requires admin-token or org-token auth"})
		return
	}
	if displaySessionSigningSecret() == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "display session signing secret is not configured"})
		return
	}
	workspaceID := c.Param("id")
	startedAt := time.Now()
	emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.started", workspaceID, map[string]any{
		"controller":    req.Controller,
		"controlled_by": controlledBy,
		"ttl_seconds":   req.TTLSeconds,
	})
	var lock workspaceDisplayControlResponse
	// Human-preempts-agent takeover (§8, checklist line 269): a user acquiring
	// control PREEMPTS an active AGENT lock — the human is the ultimate authority
	// and needs NO admin token to take the wheel (the third WHERE clause below).
	// A user still cannot steal ANOTHER user's active lock (that path 409s and
	// requires a force release). Once preempted, the gateway fails the agent's
	// next /input closed (ErrHumanInControl), so control transfers atomically.
	err := db.DB.QueryRowContext(c.Request.Context(), acquireDisplayControlQuery,
		workspaceID, req.Controller, controlledBy, req.TTLSeconds,
	).Scan(&lock.Controller, &lock.ControlledBy, &lock.ExpiresAt)
	if err == nil {
		lock.SessionURL = signedDisplaySessionURL(workspaceID, lock.ControlledBy, lock.ExpiresAt)
		emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.completed", workspaceID, map[string]any{
			"controller":    lock.Controller,
			"controlled_by": lock.ControlledBy,
			"ttl_seconds":   req.TTLSeconds,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
		})
		c.JSON(http.StatusOK, lock)
		return
	}
	if err == sql.ErrNoRows {
		current, found, loadErr := h.loadActiveDisplayControl(c, workspaceID)
		if loadErr != nil {
			log.Printf("AcquireDisplayControl: load active lock for %s failed: %v", workspaceID, loadErr)
			emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"error":         loadErr.Error(),
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
			return
		}
		emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         "display control already held",
		})
		if !found {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "display control already held",
				"current": workspaceDisplayControlNoneResponse{Controller: "none"},
			})
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "display control already held",
			"current": current,
		})
		return
	}
	log.Printf("AcquireDisplayControl: acquire lock for %s failed: %v", workspaceID, err)
	emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"duration_ms":   time.Since(startedAt).Milliseconds(),
		"error":         err.Error(),
	})
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acquire display control"})
}

// ReleaseDisplayControl handles POST /workspaces/:id/display/control/release.
func (h *WorkspaceHandler) ReleaseDisplayControl(c *gin.Context) {
	var req releaseDisplayControlRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid display control release request"})
			return
		}
	}
	if req.Force {
		if !displayControlIsAdminToken(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "force release requires admin-token auth"})
			return
		}
	}

	controlledBy, ok := displayControlActor(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control requires admin-token or org-token auth"})
		return
	}
	workspaceID := c.Param("id")
	startedAt := time.Now()
	emitDisplayControlEvent(c.Request.Context(), "display.control.release.started", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"force":         req.Force,
	})
	query := `DELETE FROM workspace_display_control_locks WHERE workspace_id = $1 AND controlled_by = $2`
	args := []interface{}{workspaceID, controlledBy}
	if req.Force {
		query = `DELETE FROM workspace_display_control_locks WHERE workspace_id = $1`
		args = []interface{}{workspaceID}
	}
	result, err := db.DB.ExecContext(c.Request.Context(), query, args...)
	if err != nil {
		log.Printf("ReleaseDisplayControl: release lock for %s failed: %v", workspaceID, err)
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         err.Error(),
			"force":         req.Force,
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to release display control"})
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("ReleaseDisplayControl: rows affected for %s failed: %v", workspaceID, err)
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         err.Error(),
			"force":         req.Force,
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to release display control"})
		return
	}
	if rowsAffected == 0 {
		current, found, loadErr := h.loadActiveDisplayControl(c, workspaceID)
		if loadErr != nil {
			log.Printf("ReleaseDisplayControl: load active lock for %s failed: %v", workspaceID, loadErr)
			emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"error":         loadErr.Error(),
				"force":         req.Force,
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
			return
		}
		if !found {
			emitDisplayControlEvent(c.Request.Context(), "display.control.release.completed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"force":         req.Force,
				"rows_affected": rowsAffected,
			})
			c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none"})
			return
		}
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         "display control held by another caller",
			"force":         req.Force,
		})
		c.JSON(http.StatusConflict, gin.H{
			"error":   "display control held by another caller",
			"current": current,
		})
		return
	}
	emitDisplayControlEvent(c.Request.Context(), "display.control.release.completed", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"duration_ms":   time.Since(startedAt).Milliseconds(),
		"force":         req.Force,
		"rows_affected": rowsAffected,
	})
	c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none"})
}

func (h *WorkspaceHandler) loadActiveDisplayControl(c *gin.Context, workspaceID string) (workspaceDisplayControlResponse, bool, error) {
	var lock workspaceDisplayControlResponse
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = $1 AND expires_at > now()`,
		workspaceID,
	).Scan(&lock.Controller, &lock.ControlledBy, &lock.ExpiresAt)
	if err == nil {
		return lock, true, nil
	}
	if err == sql.ErrNoRows {
		return workspaceDisplayControlResponse{}, false, nil
	}
	return workspaceDisplayControlResponse{}, false, err
}

func (h *WorkspaceHandler) displayControlEnabled(c *gin.Context, workspaceID string) bool {
	var raw string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COALESCE(compute, '{}'::jsonb) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return false
		}
		log.Printf("displayControlEnabled: load compute for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display config"})
		return false
	}
	compute, err := parseWorkspaceDisplayCompute(workspaceID, raw)
	if err != nil {
		log.Printf("displayControlEnabled: invalid display config for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid display config"})
		return false
	}
	if compute.Display.Mode == "" || compute.Display.Mode == "none" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "display not enabled"})
		return false
	}
	return true
}

func parseWorkspaceDisplayCompute(workspaceID, raw string) (models.WorkspaceCompute, error) {
	var compute models.WorkspaceCompute
	if raw == "" || raw == "{}" {
		return compute, nil
	}
	if err := json.Unmarshal([]byte(raw), &compute); err != nil {
		return models.WorkspaceCompute{}, fmt.Errorf("invalid compute JSON for %s: %w", workspaceID, err)
	}
	if err := validateWorkspaceDisplayConfig(compute.Display); err != nil {
		return models.WorkspaceCompute{}, err
	}
	return compute, nil
}

func displayControlActor(c *gin.Context) (string, bool) {
	if v, ok := c.Get("org_token_prefix"); ok {
		if s, ok := v.(string); ok && s != "" {
			return actorOrgTokenPrefix + s, true
		}
	}
	if v, ok := c.Get("cp_session_actor"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	if displayControlIsAdminToken(c) {
		return actorAdminToken, true
	}
	return "", false
}

func displayControlIsAdminToken(c *gin.Context) bool {
	adminSecret := os.Getenv("ADMIN_TOKEN")
	if adminSecret == "" {
		return false
	}
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	return subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1
}

func emitDisplayControlEvent(ctx context.Context, eventType string, workspaceID string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("emitDisplayControlEvent: marshal %s payload failed: %v", eventType, err)
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, workspace_id, payload, created_at)
		VALUES ($1, $2, $3::jsonb, now())
	`, eventType, workspaceID, string(payloadJSON)); err != nil {
		log.Printf("emitDisplayControlEvent: insert %s failed: %v", eventType, err)
	}
}

func signedDisplaySessionURL(workspaceID, controlledBy string, expiresAt time.Time) string {
	token := signDisplaySessionToken(workspaceID, controlledBy, expiresAt)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("/workspaces/%s/display/session/websockify#token=%s", url.PathEscape(workspaceID), token)
}

func signDisplaySessionToken(workspaceID, controlledBy string, expiresAt time.Time) string {
	key := deriveDisplayKey(workspaceID)
	if key == nil || controlledBy == "" || expiresAt.IsZero() {
		return ""
	}
	payload := strings.Join([]string{workspaceID, controlledBy, strconv.FormatInt(expiresAt.Unix(), 10)}, "|")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validateDisplaySessionToken(token, workspaceID, controlledBy string, expiresAt time.Time) bool {
	key := deriveDisplayKey(workspaceID)
	parts := strings.Split(token, ".")
	if key == nil || len(parts) != 2 || controlledBy == "" || expiresAt.IsZero() || time.Now().After(expiresAt) {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(payloadBytes)
	wantPayload := strings.Join([]string{workspaceID, controlledBy, strconv.FormatInt(expiresAt.Unix(), 10)}, "|")
	if subtle.ConstantTimeCompare([]byte(payload), []byte(wantPayload)) != 1 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return hmac.Equal(sig, mac.Sum(nil))
}

// DesktopSigningRoot resolves the root secret for ALL desktop token derivation —
// the per-sidecar control token (provisioner + gateway's DeriveDesktopControlToken)
// AND the display session/viewer tokens below. First non-empty wins:
//
//  1. DISPLAY_SESSION_SIGNING_SECRET — explicit operator override; a deployment
//     that already sets it is byte-identical (zero behavior change).
//  2. a subkey derived from SECRETS_ENCRYPTION_KEY — prod boot ALREADY refuses to
//     start without this (fail-secure, main.go), so the desktop no longer needs a
//     dedicated hand-set secret and can't be silently disabled by forgetting one.
//  3. a subkey derived from MOLECULE_CP_SHARED_SECRET / PROVISION_SHARED_SECRET
//     (the managed-tenant shared secret) as a further fallback.
//
// Returns "" only when NONE is present (a misconfigured dev env) — the same
// fail-closed "then the desktop stays disabled" contract as before, minus the
// hand-set-secret footgun. Derivation is HMAC with a domain-separation label, so
// the desktop key is cryptographically independent of the source secret's other
// uses (deriving a MAC subkey from a master key is standard KDF practice).
func DesktopSigningRoot() string {
	if s := os.Getenv("DISPLAY_SESSION_SIGNING_SECRET"); s != "" {
		return s
	}
	for _, name := range []string{"SECRETS_ENCRYPTION_KEY", "MOLECULE_CP_SHARED_SECRET", "PROVISION_SHARED_SECRET"} {
		if root := os.Getenv(name); root != "" {
			mac := hmac.New(sha256.New, []byte(root))
			_, _ = mac.Write([]byte("molecule-desktop-signing/v1"))
			return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}
	}
	return ""
}

// displaySessionSigningSecret reports the resolved desktop signing root (non-empty
// == signing is configured). Callers that need to sign/verify a specific token use
// deriveDisplayKey instead, which binds the key to the workspace.
func displaySessionSigningSecret() string {
	return DesktopSigningRoot()
}

// deriveDisplayKey returns the per-workspace HMAC key for display session/viewer
// tokens: a distinct subkey of DesktopSigningRoot, domain-separated by purpose and
// bound to the workspace so one workspace's signing key is independent of
// another's — mirroring the per-sidecar DeriveDesktopControlToken. Returns nil
// when signing is unconfigured or workspaceID is empty (callers then fail closed).
func deriveDisplayKey(workspaceID string) []byte {
	root := DesktopSigningRoot()
	if root == "" || workspaceID == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(root))
	_, _ = mac.Write([]byte("molecule-display-token/v1|" + workspaceID))
	return mac.Sum(nil)
}

// View/control split (design §8, review fix): the CONTROL token above binds to
// the lock holder (controlledBy), which is correct for /input arbitration — but
// it meant a human who does NOT hold the lock could not even WATCH (no token
// validates). Sight must never be arbitrated. signDisplayViewerToken mints a
// VIEW-ONLY token bound only to the workspace (+ a self-contained expiry), so
// any authorized caller can watch regardless of who holds control. DisplaySession
// accepts EITHER a valid viewer token OR a valid control token; only /input is
// gated on holding the lock.
// displayViewerTTL bounds a minted view-only session. Sight is low-stakes and
// re-mintable on the next DisplayControl poll, so a short window is fine.
const displayViewerTTL = 300 * time.Second

// signedDisplayViewerURL mints a VIEW-ONLY session URL for a workspace — usable
// by any authorized caller to watch regardless of who holds control (§8). Empty
// when signing is unconfigured. This is the issuance half of the view/control
// split; DisplaySession accepts the resulting token.
func signedDisplayViewerURL(workspaceID string) string {
	tok := signDisplayViewerToken(workspaceID, time.Now().Add(displayViewerTTL))
	if tok == "" {
		return ""
	}
	return fmt.Sprintf("/workspaces/%s/display/session/websockify#token=%s", url.PathEscape(workspaceID), tok)
}

func signDisplayViewerToken(workspaceID string, expiresAt time.Time) string {
	key := deriveDisplayKey(workspaceID)
	if key == nil || expiresAt.IsZero() {
		return ""
	}
	payload := strings.Join([]string{"view", workspaceID, strconv.FormatInt(expiresAt.Unix(), 10)}, "|")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validateDisplayViewerToken verifies a view-only token for workspaceID. The
// expiry lives IN the token (self-contained), so — unlike the control token —
// the validator needs NO knowledge of the lock holder or lease. This is what
// decouples watching from controlling.
func validateDisplayViewerToken(token, workspaceID string) bool {
	key := deriveDisplayKey(workspaceID)
	parts := strings.Split(token, ".")
	if key == nil || len(parts) != 2 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payloadBytes)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	fields := strings.Split(string(payloadBytes), "|")
	if len(fields) != 3 || fields[0] != "view" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(fields[1]), []byte(workspaceID)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().After(time.Unix(exp, 0)) {
		return false
	}
	return true
}
