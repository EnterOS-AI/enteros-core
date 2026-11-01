package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// continuousSecretKey is the secret behind a workspace's "24/7" switch. It
// reaches the runtime as MOLECULE_CONTINUOUS (molecule-ai-workspace-runtime
// workspace_mode.py: off = no idle self-wake; messages and schedules still
// run). Provision builds the pod env from global_secrets and then the
// workspace's own workspace_secrets rows on top (loadWorkspaceSecrets), so a
// workspace row beats a global row; with neither, the runtime default applies:
// on.
const continuousSecretKey = "MOLECULE_CONTINUOUS"

// continuousFromValue reads the value the way the runtime does
// (mailbox_dir._FALSY): only an explicitly falsy value turns it off.
func continuousFromValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// ContinuousHandler serves GET/PUT /workspaces/:id/continuous.
type ContinuousHandler struct {
	// restartFunc applies a change. Wire RestartByIDAfterMutation: RestartByID
	// drops a call made within 60 s of the previous cycle's start, which leaves
	// the pod on the old value while GET reports the new one and every repeat
	// PUT answers "unchanged".
	restartFunc func(workspaceID string)
}

func NewContinuousHandler(restartFunc func(string)) *ContinuousHandler {
	return &ContinuousHandler{restartFunc: restartFunc}
}

// continuousSetting is the switch as the workspace's next provision applies it.
type continuousSetting struct {
	on     bool
	source string // "workspace_secrets", "global_secrets" or "default"
	// inherited and inheritedSource are the value without the workspace's own
	// row: the global row, else the default.
	inherited       bool
	inheritedSource string
}

// readSecretValue decrypts the single (encrypted_value, encryption_version) row
// the query selects; found is false when there is no row.
func readSecretValue(ctx context.Context, query string, args ...interface{}) (value string, found bool, err error) {
	var enc []byte
	var ver int
	err = db.DB.QueryRowContext(ctx, query, args...).Scan(&enc, &ver)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := crypto.DecryptVersioned(enc, ver)
	if err != nil {
		return "", false, err
	}
	return string(plain), true, nil
}

func readContinuous(ctx context.Context, workspaceID string) (continuousSetting, error) {
	s := continuousSetting{on: true, source: "default", inherited: true, inheritedSource: "default"}
	v, found, err := readSecretValue(ctx,
		`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = $1`,
		continuousSecretKey)
	if err != nil {
		return s, err
	}
	if found {
		s.inherited, s.inheritedSource = continuousFromValue(v), "global_secrets"
		s.on, s.source = s.inherited, s.inheritedSource
	}
	v, found, err = readSecretValue(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
		workspaceID, continuousSecretKey)
	if err != nil {
		return s, err
	}
	if found {
		s.on, s.source = continuousFromValue(v), "workspace_secrets"
	}
	return s, nil
}

// continuousWorkspaceFound writes a 404 for an id with no workspace row, as
// SetModel does, and reports whether the handler may go on.
func continuousWorkspaceFound(c *gin.Context, workspaceID string) bool {
	var one int
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT 1 FROM workspaces WHERE id = $1`, workspaceID).Scan(&one)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return false
	}
	if err != nil {
		log.Printf("continuous: workspace lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up workspace"})
		return false
	}
	return true
}

// Get handles GET /workspaces/:id/continuous. It reports the stored setting,
// which the pod gets at its next provision.
func (h *ContinuousHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	if !continuousWorkspaceFound(c, workspaceID) {
		return
	}
	s, err := readContinuous(c.Request.Context(), workspaceID)
	if err != nil {
		log.Printf("continuous: read failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read continuous"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"continuous": s.on, "source": s.source})
}

// Set handles PUT /workspaces/:id/continuous {"continuous": bool}.
//
// Not approval-gated, like SetModel: a behavior setting, not a credential. It
// keeps the generic secret Set's guards: an admin-token caller may not write
// the org's platform root (conciergeSelfSecretWriteBlocked), and the restart
// that applies a change is skipped for the platform root and for a workspace
// writing its own switch (autoRestartAllowed). A skipped change applies at the
// next explicit restart; the response's "restart" says which happened. A PUT
// that matches the stored setting writes and restarts nothing: a restart
// re-creates the pod and wipes the agent's unpushed /workspace checkout.
func (h *ContinuousHandler) Set(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	var body struct {
		Continuous *bool `json:"continuous"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Continuous == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"continuous": true|false}`})
		return
	}
	want := *body.Continuous
	if !continuousWorkspaceFound(c, workspaceID) {
		return
	}
	if blocked, reason := conciergeSelfSecretWriteBlocked(c, workspaceID); blocked {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}
	ctx := c.Request.Context()

	s, err := readContinuous(ctx, workspaceID)
	if err != nil {
		log.Printf("continuous: read failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read continuous"})
		return
	}
	if s.on == want {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged", "continuous": want, "source": s.source})
		return
	}

	source := "workspace_secrets"
	if want == s.inherited {
		// Only the workspace row holds the other value; without it the
		// workspace inherits want.
		source = s.inheritedSource
		_, err = db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
			workspaceID, continuousSecretKey)
	} else {
		var enc []byte
		if enc, err = crypto.Encrypt([]byte(strconv.FormatBool(want))); err == nil {
			_, err = db.DB.ExecContext(ctx, `
				INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (workspace_id, key) DO UPDATE
					SET encrypted_value = $3, encryption_version = $4, updated_at = now()
			`, workspaceID, continuousSecretKey, enc, crypto.CurrentEncryptionVersion())
		}
	}
	if err != nil {
		log.Printf("continuous: write failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save continuous"})
		return
	}

	restart := "skipped"
	if h.restartFunc != nil && autoRestartAllowed(ctx, callerWorkspaceID(c), workspaceID) {
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
		restart = "started"
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "continuous": want, "source": source, "restart": restart})
}
