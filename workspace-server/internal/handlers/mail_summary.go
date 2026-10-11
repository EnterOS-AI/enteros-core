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
//   received            core#5028: per-message IDENTITY for the same unread
//                       set the two counts above aggregate — a stable row id,
//                       the a2a messageId when the sender supplied one, the
//                       sender and the method. IDS ONLY, NO BODIES: this is
//                       still the D5 "counts + a pull instruction" surface,
//                       widened by the minimum needed to tell WHICH messages
//                       are unread, not WHAT they say.
//
//                       Why this is not cosmetic: the idle digest hashes the
//                       provider's item_ids to decide whether to fire. With
//                       only a count in that tuple, a read + a fresh arrival
//                       between two ticks leaves the count — and therefore the
//                       hash — unchanged, and a brand-new inbound is NEVER
//                       surfaced (the compensating-churn hole). The sent side
//                       already carries per-delegation ids for exactly this
//                       reason; this is the received side catching up.
//
//                       Capped at mailReceivedListCap, ordered NEWEST FIRST so
//                       a fresh arrival always enters the sample even when the
//                       backlog exceeds the cap (oldest-first would let a deep
//                       backlog freeze the identity set — the same saturation
//                       bug the overdue list has). received_unread stays the
//                       uncapped total.
//   sent_awaiting_reply delegations THIS workspace sent (caller_id) still in a
//                       non-terminal status — DERIVED from
//                       DelegationInFlightStates (incl. `stuck`).
//   overdue             the sent_awaiting_reply subset older than
//                       ?overdue_after_seconds (default 21600 = 6h — the
//                       "target agent may have an issue" warning the digest
//                       lifts into the D2 urgency band). Capped at 10 entries,
//                       oldest first; ids only, no bodies.
//   overdue_count       the TRUE, UNCAPPED overdue total. The runtime has
//                       carried a `data.get("overdue_count", len(overdue))`
//                       fallback since #4308 "only for a server that predates
//                       the field" — but no server ever emitted it, so the
//                       fallback was PERMANENT and 25 overdue rendered as the
//                       capped 10. Emitting it retires the fallback.
//
// Cheap by construction: three indexed aggregate queries — the partial
// indexes idx_activity_a2a_receive_ws_seq and idx_delegations_caller_inflight
// (20260714000000_mail_summary_indexes.up.sql) key exactly on the two
// predicates below; no row bodies on the wire.

import (
	"context"
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

// mailReceivedListCap bounds the per-message identity list (core#5028). Wider
// than the overdue cap because this list is the digest's CHANGE DETECTOR, not
// a naming sample: every id inside the cap is one more inbound whose arrival
// or consumption can re-fire the digest. Still ids only — no bodies — so the
// row stays a few hundred bytes.
const mailReceivedListCap = 25

// MailSummaryHandler serves the idle-digest mail counts.
type MailSummaryHandler struct{}

func NewMailSummaryHandler() *MailSummaryHandler { return &MailSummaryHandler{} }

type mailOverdueEntry struct {
	DelegationID string `json:"delegation_id"`
	TargetID     string `json:"target_workspace_id"`
	AgeSeconds   int64  `json:"age_seconds"`
}

// mailReceivedEntry is ONE unread inbound's identity (core#5028).
//
// ID is ALWAYS present and stable for the life of the row — the activity_logs
// PK in acked_seq mode, the a2a_queue PK in queued_backlog mode — so a consumer
// never has to invent an identity. MessageID is the a2a params.message.messageId
// and is only present when the sender supplied one; it is the id a tenant-side
// reconciler correlates against its own ledger, which is why it is projected
// SEPARATELY rather than folded into ID (an id that is sometimes a messageId and
// sometimes a row PK cannot be correlated against anything).
type mailReceivedEntry struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id,omitempty"`
	SenderID  string `json:"sender_id,omitempty"`
	Method    string `json:"method,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
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

	// core#5028: the SAME unread set the counts above aggregate, projected as
	// per-message IDENTITIES (ids only, no bodies). Same predicate, same index,
	// NEWEST FIRST + LIMIT — so a fresh arrival always lands in the sample.
	//
	// Reuses columns that already exist: activity_logs.message_id (written by
	// LogActivity's messageId-keyed conflict path) and a2a_queue.body's
	// params.message.messageId. No new store, no new column, no new write path.
	received, err := h.receivedIdentities(ctx, wsID, mode, floor)
	if err != nil {
		log.Printf("mail summary: received-identity query failed for %s (mode=%s): %v", wsID, mode, err)
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

	var sentAwaiting, overdueTotal int64
	overdue := make([]mailOverdueEntry, 0, mailOverdueListCap)
	for rows.Next() {
		var id, callee string
		var age int64
		if err := rows.Scan(&id, &callee, &age); err != nil {
			log.Printf("mail summary: delegation row scan failed for %s: %v", wsID, err)
			continue
		}
		sentAwaiting++
		if age >= overdueAfter {
			// overdueTotal counts EVERY overdue row; `overdue` is only the
			// capped naming sample. Counting inside the cap check is what made
			// len(overdue) the only available total — and the runtime's
			// "predates overdue_count" fallback permanent.
			overdueTotal++
			if len(overdue) < mailOverdueListCap {
				overdue = append(overdue, mailOverdueEntry{DelegationID: id, TargetID: callee, AgeSeconds: age})
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("mail summary: delegations iteration failed for %s: %v", wsID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mail summary query failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"received_unread": receivedUnread,
		"replies_unread":  repliesUnread,
		// core#5028: ALWAYS present (possibly empty) once this server is
		// deployed. The runtime distinguishes "key absent" (old server —
		// degrade to counts and SAY SO) from "key present but empty" (nothing
		// unread), so the field's PRESENCE is the version handshake. Do not
		// make it omitempty.
		"received":            received,
		"received_list_cap":   mailReceivedListCap,
		"sent_awaiting_reply": sentAwaiting,
		"overdue":             overdue,
		// The TRUE uncapped total. `overdue` is a capped naming sample; a
		// consumer rendering len(overdue) under-reports the blast radius.
		"overdue_count":         overdueTotal,
		"overdue_after_seconds": overdueAfter,
		"mode":                  mode,
	})
}

// receivedIdentities projects the per-message identity of each unread inbound
// (core#5028), using the SAME predicate as the count queries above so the two
// can never describe different sets.
//
// Ordering is NEWEST FIRST on purpose. The list is capped, and the digest hashes
// it: oldest-first would let a backlog deeper than the cap pin the identity set
// forever, so a fresh arrival would change nothing — exactly the saturation
// failure the capped `overdue` list has. Newest-first guarantees a new inbound
// always enters the sample.
func (h *MailSummaryHandler) receivedIdentities(ctx context.Context, wsID, mode string, floor int64) ([]mailReceivedEntry, error) {
	var rows *sql.Rows
	var err error
	if mode == "acked_seq" {
		// Served by idx_activity_a2a_receive_ws_seq (workspace_id, seq)
		// WHERE activity_type='a2a_receive' — the same partial index the
		// count query uses; DESC + LIMIT is an index-order walk.
		rows, err = db.DB.QueryContext(ctx, `
			SELECT id::text,
			       COALESCE(message_id, ''),
			       COALESCE(source_id::text, ''),
			       COALESCE(method, ''),
			       seq
			FROM activity_logs
			WHERE workspace_id = $1 AND activity_type = 'a2a_receive' AND seq > $2
			ORDER BY seq DESC
			LIMIT $3`, wsID, floor, mailReceivedListCap)
	} else {
		// Push fleet: the unread set is the platform-queued backlog. a2a_queue
		// has no message_id COLUMN, but the envelope is already in `body` —
		// read the same params.message.messageId the activity path extracts in
		// Go (extractMessageIdFromA2ABody). No new column, no backfill.
		rows, err = db.DB.QueryContext(ctx, `
			SELECT id::text,
			       COALESCE(body->'params'->'message'->>'messageId', ''),
			       COALESCE(caller_id::text, ''),
			       COALESCE(method, '')
			FROM a2a_queue
			WHERE workspace_id = $1 AND status = 'queued'
			ORDER BY enqueued_at DESC, id DESC
			LIMIT $2`, wsID, mailReceivedListCap)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so the JSON is `[]`, never `null` — the runtime's presence check
	// keys on the field existing, and `null` would read as an absent list.
	out := make([]mailReceivedEntry, 0, mailReceivedListCap)
	for rows.Next() {
		var e mailReceivedEntry
		if mode == "acked_seq" {
			err = rows.Scan(&e.ID, &e.MessageID, &e.SenderID, &e.Method, &e.Seq)
		} else {
			err = rows.Scan(&e.ID, &e.MessageID, &e.SenderID, &e.Method)
		}
		if err != nil {
			log.Printf("mail summary: received row scan failed for %s: %v", wsID, err)
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
