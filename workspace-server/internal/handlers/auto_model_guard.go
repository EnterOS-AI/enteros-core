package handlers

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// Auto model routing (platform/auto) is billed to platform credits. A workspace
// that holds its own direct vendor key (BYOK) must not select it: the two
// billing modes would be mixed. The guard is model-id based and does not depend
// on the SDK registry pin.

const (
	autoRequiresPlatformCreditsCode = "AUTO_REQUIRES_PLATFORM_CREDITS"
	autoRequiresPlatformCreditsMsg  = "Auto uses platform credits. Remove your provider API key or choose a specific model."
)

// isAutoModel reports whether model is the auto-routing sentinel
// ("platform/auto" or bare "auto"), trimmed. Same semantics as the CP's
// providers.IsAutoModel.
func isAutoModel(model string) bool {
	m := strings.TrimSpace(model)
	return m == "platform/auto" || m == "auto"
}

// anyDirectVendorKey reports whether any of keys is on the direct-LLM bypass list.
func anyDirectVendorKey(keys []string) bool {
	for _, k := range keys {
		if isPlatformManagedDirectLLMBypassKey(k) {
			return true
		}
	}
	return false
}

// workspaceHoldsDirectVendorKey reports whether the workspace has a stored
// direct vendor key. A failed scan fails open (logged): the provision path
// strips vendor keys from platform-routed workspaces, so this is not the only
// line of defence.
func workspaceHoldsDirectVendorKey(ctx context.Context, workspaceID string) bool {
	rows, err := db.DB.QueryContext(ctx, `SELECT key FROM workspace_secrets WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		log.Printf("auto-model-guard: workspace_secrets key scan failed for %s (failing open): %v", workspaceID, err)
		return false
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			keys = append(keys, k)
		}
	}
	return anyDirectVendorKey(keys)
}

func respondAutoRequiresPlatformCredits(c *gin.Context, runtime, model string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":   autoRequiresPlatformCreditsMsg,
		"runtime": runtime,
		"model":   model,
		"code":    autoRequiresPlatformCreditsCode,
	})
}
