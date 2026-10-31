package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// continuousSecretKey is the workspace_secrets row behind a workspace's "24/7"
// switch. Provision copies every workspace_secrets row into the pod env, so the
// row reaches the runtime as MOLECULE_CONTINUOUS (molecule-ai-workspace-runtime
// workspace_mode.py: off = no idle self-wake; messages and schedules still run).
// No row means on, the runtime default; the row exists only to turn it off.
const continuousSecretKey = "MOLECULE_CONTINUOUS"

// continuousFromValue reads the value the way the runtime does: only an
// explicitly falsy value turns it off.
func continuousFromValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// readContinuous returns the workspace's switch and whether a row set it.
func readContinuous(ctx context.Context, workspaceID string) (on bool, fromRow bool, err error) {
	var enc []byte
	var ver int
	err = db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
		workspaceID, continuousSecretKey).Scan(&enc, &ver)
	if err == sql.ErrNoRows {
		return true, false, nil
	}
	if err != nil {
		return true, false, err
	}
	plain, err := crypto.DecryptVersioned(enc, ver)
	if err != nil {
		return true, false, err
	}
	return continuousFromValue(string(plain)), true, nil
}

// GetContinuous handles GET /workspaces/:id/continuous.
func (h *SecretsHandler) GetContinuous(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	on, fromRow, err := readContinuous(c.Request.Context(), workspaceID)
	if err != nil {
		log.Printf("GetContinuous: read failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read continuous"})
		return
	}
	source := "default"
	if fromRow {
		source = "workspace_secrets"
	}
	c.JSON(http.StatusOK, gin.H{"continuous": on, "source": source})
}

// SetContinuous handles PUT /workspaces/:id/continuous {"continuous": bool}.
//
// Not approval-gated, like SetModel: a behavior setting, not a credential.
// A change restarts the workspace because the runtime reads the env at boot.
// A PUT that matches the current value writes and restarts nothing — a restart
// re-creates the pod and wipes the agent's unpushed /workspace checkout.
func (h *SecretsHandler) SetContinuous(c *gin.Context) {
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
	ctx := c.Request.Context()

	current, _, err := readContinuous(ctx, workspaceID)
	if err != nil {
		log.Printf("SetContinuous: read failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read continuous"})
		return
	}
	if current == want {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged", "continuous": want})
		return
	}

	if want {
		_, err = db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
			workspaceID, continuousSecretKey)
	} else {
		var enc []byte
		if enc, err = crypto.Encrypt([]byte("false")); err == nil {
			_, err = db.DB.ExecContext(ctx, `
				INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (workspace_id, key) DO UPDATE
					SET encrypted_value = $3, encryption_version = $4, updated_at = now()
			`, workspaceID, continuousSecretKey, enc, crypto.CurrentEncryptionVersion())
		}
	}
	if err != nil {
		log.Printf("SetContinuous: write failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save continuous"})
		return
	}

	if h.restartFunc != nil {
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "continuous": want})
}
