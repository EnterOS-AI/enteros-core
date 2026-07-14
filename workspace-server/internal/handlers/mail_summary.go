package handlers

// mail_summary.go — GET /workspaces/:id/mail/summary (task #219 phase-2, D5
// ruling 2026-07-14, issue #4308).
//
// The idle digest's mail section renders COUNTS + a pull instruction ("use the
// workspace communication MCP to see detail") — never message bodies. Since
// the mailbox kernel is native, sent/received are WORKSPACE-DB state: this
// endpoint is a thin aggregate over the ledgers the platform already owns —
// no new store, no duplicate SSOT (the design's no-duplicate-platform-ledger
// rule):
//
//   received_unread     inbound a2a_receive rows the agent has not consumed.
//                       Two read-marker modes, reported in `mode`:
//                         acked_seq      — the workspace acks its inbox (poll
//                                          delivery / standalone molecule-mcp):
//                                          rows with seq > inbox_delivery_state
//                                          .last_acked_seq (core#3373 floor).
//                         queued_backlog — push delivery (the container fleet
//                                          never acks): a pushed row is
//                                          consumed by the very turn that
//                                          delivered it, so "unread" is the
//                                          platform-queued-but-undelivered
//                                          backlog (a2a_queue status='queued').
//   replies_unread      same two modes, restricted to method='delegate_result'
//                       (pushDelegationResultToInbox writes the caller's reply
//                       rows through the same inbox, so the same floor covers
//                       them).
//   sent_awaiting_reply delegations THIS workspace sent (caller_id) still in a
//                       non-terminal status — DERIVED from
//                       DelegationInFlightStates (incl. `stuck`).
//   overdue             the sent_awaiting_reply subset older than
//                       ?overdue_after_seconds (default 21600 = 6h — the
//                       "target agent may have an issue" warning the digest
//                       lifts into the D2 urgency band). Capped at 10 entries,
//                       oldest first; ids only, no bodies.
//
// Cheap by construction: three indexed aggregate queries — the partial
// indexes idx_activity_a2a_receive_ws_seq and idx_delegations_caller_inflight
// (20260714000000_mail_summary_indexes.up.sql) key exactly on the two
// predicates below; no row bodies on the wire.

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// DefaultMailOverdueAfterSeconds is the default no-reply age beyond which a
// sent delegation is flagged overdue (6h — matches the delegations table's
// default hard deadline, so "overdue" and "sweeper-failed" line up).
const DefaultMailOverdueAfterSeconds = 21600

// mailOverdueListCap bounds the overdue id list in the response — the digest
// names a few offenders, it does not page the ledger.
const mailOverdueListCap = 10

// MailSummaryHandler serves the idle-digest mail counts.
type MailSummaryHandler struct{}

func NewMailSummaryHandler() *MailSummaryHandler { return &MailSummaryHandler{} }

type mailOverdueEntry struct {
	DelegationID string `json:"delegation_id"`
	TargetID     string `json:"target_workspace_id"`
	AgeSeconds   int64  `json:"age_seconds"`
}

// Summary handles GET /workspaces/:id/mail/summary.
func (h *MailSummaryHandler) Summary(c *gin.Context) {
	ctx := c.Request.Context()
	wsID := c.Param("id")

	overdueAfter := int64(DefaultMailOverdueAfterSeconds)
	if raw := c.Query("overdue_after_seconds"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 60 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "overdue_after_seconds must be an integer >= 60"})
			return
		}
		overdueAfter = v
	}

	// Read-marker mode: an inbox_delivery_state row means this workspace acks
	// its inbox (poll delivery) and the seq floor is authoritative.
	var floor int64
	mode := "acked_seq"
	err := db.DB.QueryRowContext(ctx,
		`SELECT last_acked_seq FROM inbox_delivery_state WHERE workspace_id = $1`,
		wsID).Scan(&floor)
	switch err {
	case nil:
	case sql.ErrNoRows:
		mode = "queued_backlog"
	default:
		log.Printf("mail summary: floor query failed for %s: %v", wsID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mail summary query failed"})
		return
	}

	var receivedUnread, repliesUnread int64
	if mode == "acked_seq" {
		err = db.DB.QueryRowContext(ctx, `
			SELECT
			  COUNT(*) FILTER (WHERE COALESCE(method,'') <> 'delegate_result'),
			  COUNT(*) FILTER (WHERE method = 'delegate_result')
			FROM activity_logs
			WHERE workspace_id = $1 AND activity_type = 'a2a_receive' AND seq > $2`,
			wsID, floor).Scan(&receivedUnread, &repliesUnread)
	} else {
		err = db.DB.QueryRowContext(ctx, `
			SELECT
			  COUNT(*) FILTER (WHERE COALESCE(method,'') <> 'delegate_result'),
			  COUNT(*) FILTER (WHERE method = 'delegate_result')
			FROM a2a_queue
			WHERE workspace_id = $1 AND status = 'queued'`,
			wsID).Scan(&receivedUnread, &repliesUnread)
	}
	if err != nil {
		log.Printf("mail summary: unread query failed for %s (mode=%s): %v", wsID, mode, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mail summary query failed"})
		return
	}

	// Sent-awaiting-reply + the overdue subset, oldest first. One indexed scan
	// (idx_delegations_caller_created); ages computed DB-side so the handler
	// has no clock skew vs created_at.
	// The IN-list is DERIVED (delegation_ledger.go), never hand-typed. This very
	// query with a hand-typed list IS bug #4314: the sweeper writes `stuck`, the
	// list didn't have it, and a wedged delegation silently dropped out of the
	// caller's "awaiting reply" count — hiding the one case the ⚠ warning exists to
	// surface. `stuck` is IN-FLIGHT: the target has not answered.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT delegation_id, callee_id,
		       EXTRACT(EPOCH FROM (now() - created_at))::bigint AS age_seconds
		FROM delegations
		WHERE caller_id = $1 AND status IN (`+sqlInFlightStates()+`)
		ORDER BY created_at ASC`,
		wsID)
	if err != nil {
		log.Printf("mail summary: delegations query failed for %s: %v", wsID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mail summary query failed"})
		return
	}
	defer rows.Close()

	var sentAwaiting int64
	overdue := make([]mailOverdueEntry, 0, mailOverdueListCap)
	for rows.Next() {
		var id, callee string
		var age int64
		if err := rows.Scan(&id, &callee, &age); err != nil {
			log.Printf("mail summary: delegation row scan failed for %s: %v", wsID, err)
			continue
		}
		sentAwaiting++
		if age >= overdueAfter && len(overdue) < mailOverdueListCap {
			overdue = append(overdue, mailOverdueEntry{DelegationID: id, TargetID: callee, AgeSeconds: age})
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("mail summary: delegations iteration failed for %s: %v", wsID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mail summary query failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"received_unread":       receivedUnread,
		"replies_unread":        repliesUnread,
		"sent_awaiting_reply":   sentAwaiting,
		"overdue":               overdue,
		"overdue_after_seconds": overdueAfter,
		"mode":                  mode,
	})
}
