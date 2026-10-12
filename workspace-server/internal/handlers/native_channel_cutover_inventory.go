package handlers

import (
	"database/sql"
	"log"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
)

// NativeChannelCutoverInventoryHandler is a temporary, read-only migration
// surface for the native-channel-to-plugin cutover. It must be removed with the
// native subsystem in molecule-core#4267 after the fleet evidence is accepted.
// It intentionally exposes counts and workspace identifiers only; channel
// configuration and credential-bearing columns are never selected.
type NativeChannelCutoverInventoryHandler struct {
	db *sql.DB
}

// NewNativeChannelCutoverInventoryHandler constructs the temporary cutover
// inventory handler with the tenant's own database.
func NewNativeChannelCutoverInventoryHandler(database *sql.DB) *NativeChannelCutoverInventoryHandler {
	return &NativeChannelCutoverInventoryHandler{db: database}
}

type nativeChannelCutoverWorkspaceCount struct {
	WorkspaceID string `json:"workspace_id"`
	RowCount    int64  `json:"row_count"`
}

type nativeChannelCutoverInventoryResponse struct {
	ContractVersion int                                  `json:"contract_version"`
	TableState      string                               `json:"table_state"`
	TotalRows       int64                                `json:"total_rows"`
	OrphanRows      int64                                `json:"orphan_rows"`
	WorkspaceRowSum int64                                `json:"workspace_row_sum"`
	Workspaces      []nativeChannelCutoverWorkspaceCount `json:"workspaces"`
}

const nativeChannelCutoverTablePresentQuery = `
	SELECT to_regclass('public.workspace_channels') IS NOT NULL`

// Rows whose workspace is missing or removed are deliberately classified as
// orphans. They are outside GET /workspaces' active roster but still block a
// zero-row cutover and must remain part of the global accounting identity.
const nativeChannelCutoverTotalsQuery = `
	SELECT COUNT(*)::bigint, COUNT(*) FILTER (
		WHERE NOT EXISTS (
			SELECT 1
			FROM workspaces w
			WHERE w.id = wc.workspace_id
			  AND w.status != 'removed'
		)
	)::bigint
	FROM workspace_channels wc`

// The LEFT JOIN is intentional: every active workspace is returned even when
// its legacy row count is zero. Ordering makes repeated snapshots stable.
const nativeChannelCutoverWorkspaceCountsQuery = `
	SELECT w.id, COUNT(wc.id)::bigint
	FROM workspaces w
	LEFT JOIN workspace_channels wc ON wc.workspace_id = w.id
	WHERE w.status != 'removed'
	GROUP BY w.id
	ORDER BY w.id`

// Inventory handles GET /admin/cutovers/native-channels/inventory.
//
// Router wiring applies AdminAuth. All reads run in one read-only,
// repeatable-read transaction so table state, global totals, orphan totals,
// and the per-workspace list describe one database snapshot. Any query, scan,
// iteration, accounting, or commit error rejects the entire response rather
// than returning a valid-looking partial inventory.
func (h *NativeChannelCutoverInventoryHandler) Inventory(c *gin.Context) {
	if h == nil || h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}

	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		log.Printf("native channel cutover inventory: begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var tablePresent bool
	if err := tx.QueryRowContext(c.Request.Context(), nativeChannelCutoverTablePresentQuery).Scan(&tablePresent); err != nil {
		log.Printf("native channel cutover inventory: table-state query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}
	if !tablePresent {
		c.JSON(http.StatusConflict, gin.H{
			"contract_version": 1,
			"table_state":      "absent",
			"error":            "workspace_channels is absent before the native-channel cutover",
		})
		return
	}

	var totalRows, orphanRows int64
	if err := tx.QueryRowContext(c.Request.Context(), nativeChannelCutoverTotalsQuery).Scan(&totalRows, &orphanRows); err != nil {
		log.Printf("native channel cutover inventory: totals query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}
	if totalRows < 0 || orphanRows < 0 || orphanRows > totalRows {
		log.Printf("native channel cutover inventory: invalid totals")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}

	rows, err := tx.QueryContext(c.Request.Context(), nativeChannelCutoverWorkspaceCountsQuery)
	if err != nil {
		log.Printf("native channel cutover inventory: workspace query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}

	workspaces := make([]nativeChannelCutoverWorkspaceCount, 0)
	seen := make(map[string]struct{})
	var workspaceRowSum int64
	for rows.Next() {
		var entry nativeChannelCutoverWorkspaceCount
		if err := rows.Scan(&entry.WorkspaceID, &entry.RowCount); err != nil {
			_ = rows.Close()
			log.Printf("native channel cutover inventory: workspace scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
			return
		}
		if entry.WorkspaceID == "" || entry.RowCount < 0 {
			_ = rows.Close()
			log.Printf("native channel cutover inventory: invalid workspace row")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
			return
		}
		if _, duplicate := seen[entry.WorkspaceID]; duplicate {
			_ = rows.Close()
			log.Printf("native channel cutover inventory: duplicate workspace id")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
			return
		}
		if workspaceRowSum > math.MaxInt64-entry.RowCount {
			_ = rows.Close()
			log.Printf("native channel cutover inventory: workspace sum overflow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
			return
		}
		seen[entry.WorkspaceID] = struct{}{}
		workspaceRowSum += entry.RowCount
		workspaces = append(workspaces, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Printf("native channel cutover inventory: workspace iteration failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}
	if err := rows.Close(); err != nil {
		log.Printf("native channel cutover inventory: workspace rows close failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}

	if workspaceRowSum > math.MaxInt64-orphanRows || workspaceRowSum+orphanRows != totalRows {
		log.Printf("native channel cutover inventory: accounting mismatch")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("native channel cutover inventory: commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "native channel cutover inventory unavailable"})
		return
	}

	c.JSON(http.StatusOK, nativeChannelCutoverInventoryResponse{
		ContractVersion: 1,
		TableState:      "present",
		TotalRows:       totalRows,
		OrphanRows:      orphanRows,
		WorkspaceRowSum: workspaceRowSum,
		Workspaces:      workspaces,
	})
}
