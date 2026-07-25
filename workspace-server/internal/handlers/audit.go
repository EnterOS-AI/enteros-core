package handlers

// AuditHandler implements GET /workspaces/:id/audit.
//
// It queries stored HMAC-linked audit event rows for a workspace and optionally
// verifies the returned rows inline. This is a read-only surface; event
// production is owned separately.
//
// Route (behind WorkspaceAuth middleware):
//
//	GET /workspaces/:id/audit
//
// Query parameters:
//
//	agent_id   — filter by agent ID
//	session_id — filter by session/conversation ID
//	from       — ISO 8601 / RFC 3339 lower bound on timestamp (inclusive)
//	to         — ISO 8601 / RFC 3339 upper bound on timestamp (exclusive)
//	limit      — max rows returned (default 100, max 500)
//	offset     — pagination offset (default 0)
//
// Response:
//
//	{
//	    "events":             [...],   // slice of audit event rows
//	    "total":              N,       // total matching rows (ignoring limit/offset)
//	    "chain_valid":        true|false|null,
//	    "chain_verification": "verified"|"tampered"|
//	                          "unavailable_partial_query"|"disabled_no_salt"
//	}
//
// Chain verification
// ------------------
// When AUDIT_LEDGER_SALT is set, the handler re-derives the PBKDF2 key and
// verifies every HMAC in the result set (scoped to each queried agent_id, in
// chronological order). chain_valid is null when the salt is absent or when
// pagination/session/lower-time filters omit possible chain predecessors —
// chain_verification SPLITS those two cases so an unset salt (a
// misconfiguration that leaves the ledger NOT tamper-evident) is never
// indistinguishable from a benign partial view. An unset salt is additionally
// logged loudly (once) via getAuditHMACKey; a genuine tamper is fail-closed to
// chain_valid=false / chain_verification="tampered".
//
// Environment variables:
//
//	AUDIT_LEDGER_SALT — secret salt for PBKDF2 key derivation. When unset,
//	                    chain_valid is null, chain_verification is
//	                    "disabled_no_salt", and a loud SECURITY/AUDIT log fires.

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/pbkdf2"
)

// PBKDF2 parameters are part of the stored audit-event signing contract. Any
// future producer must use these values and computeAuditHMAC's canonical form.
var (
	auditPBKDF2Salt       = []byte("molecule-audit-ledger-v1")
	auditPBKDF2Iterations = 210_000
	auditPBKDF2KeyLen     = 32

	auditKeyOnce sync.Once
	auditHMACKey []byte // nil when AUDIT_LEDGER_SALT is unset
)

// getAuditHMACKey derives (and caches) the 32-byte HMAC key from AUDIT_LEDGER_SALT.
// Returns nil when the env var is not set.
//
// A missing salt is a MISCONFIGURATION, not a benign default: with no key the
// HMAC audit-chain cannot be verified at all, so the ledger stops being
// tamper-evident. That must never be a SILENT no-audit. This function therefore
// emits a loud, once-only SECURITY/AUDIT log the first time it observes an unset
// salt, and the read handler additionally surfaces chain_verification=
// "disabled_no_salt" on EVERY response so operators and clients both see it.
func getAuditHMACKey() []byte {
	auditKeyOnce.Do(func() {
		salt := os.Getenv("AUDIT_LEDGER_SALT")
		if salt == "" {
			// Loud, not silent. Once-only (guarded by auditKeyOnce) so it cannot be
			// used to flood logs, but unmissable in operator dashboards.
			log.Printf("SECURITY/AUDIT: AUDIT_LEDGER_SALT is not set — HMAC audit-chain " +
				"verification is DISABLED; the audit ledger is NOT tamper-evident. Configure " +
				"AUDIT_LEDGER_SALT to enable append-only chain verification.")
			return
		}
		auditHMACKey = pbkdf2.Key(
			[]byte(salt),
			auditPBKDF2Salt,
			auditPBKDF2Iterations,
			auditPBKDF2KeyLen,
			sha256.New,
		)
	})
	return auditHMACKey
}

// AuditHandler queries the audit_events table.
type AuditHandler struct{}

// NewAuditHandler returns an AuditHandler (stateless — all deps via db package).
func NewAuditHandler() *AuditHandler {
	return &AuditHandler{}
}

// auditEventRow mirrors the audit_events DB columns for JSON serialisation.
type auditEventRow struct {
	ID                 string    `json:"id"`
	Timestamp          time.Time `json:"timestamp"`
	AgentID            string    `json:"agent_id"`
	SessionID          string    `json:"session_id"`
	Operation          string    `json:"operation"`
	InputHash          *string   `json:"input_hash"`
	OutputHash         *string   `json:"output_hash"`
	ModelUsed          *string   `json:"model_used"`
	HumanOversightFlag bool      `json:"human_oversight_flag"`
	RiskFlag           bool      `json:"risk_flag"`
	PrevHMAC           *string   `json:"prev_hmac"`
	HMAC               string    `json:"hmac"`
	WorkspaceID        string    `json:"workspace_id"`
}

// Query handles GET /workspaces/:id/audit.
func (h *AuditHandler) Query(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Parse query parameters ------------------------------------------------
	agentID := c.Query("agent_id")
	sessionID := c.Query("session_id")
	fromStr := c.Query("from")
	toStr := c.Query("to")

	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Build parameterized WHERE clause --------------------------------------
	where := "WHERE workspace_id = $1"
	args := []interface{}{workspaceID}
	idx := 2

	if agentID != "" {
		where += fmt.Sprintf(" AND agent_id = $%d", idx)
		args = append(args, agentID)
		idx++
	}
	if sessionID != "" {
		where += fmt.Sprintf(" AND session_id = $%d", idx)
		args = append(args, sessionID)
		idx++
	}
	if fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "from must be RFC 3339 (e.g. 2026-04-17T00:00:00Z)"})
			return
		}
		where += fmt.Sprintf(" AND timestamp >= $%d", idx)
		args = append(args, t)
		idx++
	}
	if toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "to must be RFC 3339 (e.g. 2026-04-17T23:59:59Z)"})
			return
		}
		where += fmt.Sprintf(" AND timestamp < $%d", idx)
		args = append(args, t)
		idx++
	}

	// Count total matching rows (for pagination) ----------------------------
	countQuery := "SELECT COUNT(*) FROM audit_events " + where
	var total int
	if err := db.DB.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		log.Printf("audit: count query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Fetch rows ------------------------------------------------------------
	selectQuery := `SELECT id, timestamp, agent_id, session_id, operation,
		input_hash, output_hash, model_used,
		human_oversight_flag, risk_flag, prev_hmac, hmac, workspace_id
		FROM audit_events ` + where +
		fmt.Sprintf(" ORDER BY timestamp ASC, id ASC LIMIT $%d OFFSET $%d", idx, idx+1)

	rows, err := db.DB.QueryContext(ctx, selectQuery, append(args, limit, offset)...)
	if err != nil {
		log.Printf("audit: query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	events, err := scanAuditRows(rows)
	if err != nil {
		log.Printf("audit: scan failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "scan failed"})
		return
	}
	if err := rows.Err(); err != nil {
		log.Printf("audit: rows error for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "scan failed"})
		return
	}

	// Chain verification (inline when AUDIT_LEDGER_SALT is set) ------------
	// chain_valid stays a bool|null as before. chain_verification is an added,
	// non-breaking field that SPLITS the two very different reasons chain_valid
	// can be null, so a missing salt is never a silent no-audit:
	//
	//   "verified"                  — full chain re-verified, HMAC intact
	//   "tampered"                  — HMAC/link mismatch in a complete prefix
	//   "unavailable_partial_query" — salt IS set, but offset/session/from could
	//                                 omit chain predecessors (legitimate)
	//   "disabled_no_salt"          — AUDIT_LEDGER_SALT is unset: verification is
	//                                 OFF and the ledger is NOT tamper-evident
	//                                 (a misconfiguration, also logged loudly)
	//
	// A non-zero offset, session filter, or lower time bound can omit earlier
	// events in an agent's chain, so those are reported as unavailable rather
	// than a misleading verdict. agent_id selects complete per-agent chains; an
	// upper time bound and limit retain a verifiable prefix when offset is zero.
	var chainValid *bool
	var chainVerification string
	switch {
	case getAuditHMACKey() == nil:
		// No key can be derived — the ledger is not tamper-evident. This is a
		// configuration failure, distinct from the benign partial-view case, and
		// must be surfaced on every response (getAuditHMACKey also logs loudly).
		chainVerification = "disabled_no_salt"
	case offset != 0 || sessionID != "" || fromStr != "":
		chainVerification = "unavailable_partial_query"
	default:
		chainValid = verifyAuditChain(events)
		switch {
		case chainValid == nil:
			// Unreachable while the salt is set, but stay explicit rather than
			// silently emitting a bare null.
			chainVerification = "unavailable"
		case *chainValid:
			chainVerification = "verified"
		default:
			chainVerification = "tampered"
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"events":             events,
		"total":              total,
		"chain_valid":        chainValid,
		"chain_verification": chainVerification,
	})
}

// scanAuditRows reads all rows from a *sql.Rows into a slice.
func scanAuditRows(rows *sql.Rows) ([]auditEventRow, error) {
	result := make([]auditEventRow, 0)
	for rows.Next() {
		var ev auditEventRow
		if err := rows.Scan(
			&ev.ID,
			&ev.Timestamp,
			&ev.AgentID,
			&ev.SessionID,
			&ev.Operation,
			&ev.InputHash,
			&ev.OutputHash,
			&ev.ModelUsed,
			&ev.HumanOversightFlag,
			&ev.RiskFlag,
			&ev.PrevHMAC,
			&ev.HMAC,
			&ev.WorkspaceID,
		); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// verifyAuditChain verifies the HMAC chain across the supplied events.
//
// Returns nil when AUDIT_LEDGER_SALT is not configured (chain_valid: null in
// the response).
// Returns a pointer to true/false otherwise.
func verifyAuditChain(events []auditEventRow) *bool {
	key := getAuditHMACKey()
	if key == nil {
		return nil // AUDIT_LEDGER_SALT not set — cannot verify
	}

	// Group events by agent_id and verify each agent's chain independently.
	type chainState struct {
		prevHMAC *string
	}
	chains := map[string]*chainState{}

	for i := range events {
		ev := &events[i]
		state, ok := chains[ev.AgentID]
		if !ok {
			state = &chainState{}
			chains[ev.AgentID] = state
		}

		// Recompute the expected HMAC.
		expected := computeAuditHMAC(key, ev)
		if !hmac.Equal([]byte(ev.HMAC), []byte(expected)) {
			// Truncate for logging only after confirming the slice is safe.
			storedPrefix := ev.HMAC
			computedPrefix := expected
			if len(storedPrefix) > 12 {
				storedPrefix = storedPrefix[:12]
			}
			if len(computedPrefix) > 12 {
				computedPrefix = computedPrefix[:12]
			}
			log.Printf(
				"audit: HMAC mismatch at event %s (agent=%s): stored=%q computed=%q",
				ev.ID, ev.AgentID, storedPrefix, computedPrefix,
			)
			f := false
			return &f
		}

		// Check chain linkage (constant-time to prevent HMAC oracle timing attacks).
		prevMatches := (state.prevHMAC == nil && ev.PrevHMAC == nil) ||
			(state.prevHMAC != nil && ev.PrevHMAC != nil && hmac.Equal([]byte(*state.prevHMAC), []byte(*ev.PrevHMAC)))
		if !prevMatches {
			log.Printf(
				"audit: chain break at event %s (agent=%s)",
				ev.ID, ev.AgentID,
			)
			f := false
			return &f
		}

		h := ev.HMAC
		state.prevHMAC = &h
	}

	t := true
	return &t
}

// computeAuditHMAC replicates Python's _compute_event_hmac() for a single row.
//
// Canonical JSON rules (must match ledger.py exactly):
//   - All fields except "hmac", serialised as a JSON object
//   - Keys sorted alphabetically (encoding/json.Marshal on map does this)
//   - Compact separators (no spaces)
//   - Timestamp as RFC-3339 seconds-precision with Z suffix
//   - Null values as JSON null (Go *string nil → null)
func computeAuditHMAC(key []byte, ev *auditEventRow) string {
	// Build the canonical map — keys must sort alphabetically to match Python.
	canonical := map[string]interface{}{
		"agent_id":             ev.AgentID,
		"human_oversight_flag": ev.HumanOversightFlag,
		"id":                   ev.ID,
		"input_hash":           nilOrString(ev.InputHash),
		"model_used":           nilOrString(ev.ModelUsed),
		"operation":            ev.Operation,
		"output_hash":          nilOrString(ev.OutputHash),
		"prev_hmac":            nilOrString(ev.PrevHMAC),
		"risk_flag":            ev.RiskFlag,
		"session_id":           ev.SessionID,
		"timestamp":            ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}

	payload, marshalErr := json.Marshal(canonical) // compact, sorted keys
	if marshalErr != nil {
		log.Printf("auditChainHash: json.Marshal canonical failed: %v", marshalErr)
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// nilOrString converts a *string to interface{} where nil → nil (JSON null).
func nilOrString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
