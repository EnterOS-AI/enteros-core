package handlers

// Audit-ledger PRODUCER for the audit_events table.
//
// # Why this file exists
//
// audit_events shipped with a migration (030), a read+verify surface
// (audit.go), a canvas panel, a blog post and an API doc — and NO writer.
// `git grep "INSERT INTO audit_events"` over the whole repository returned
// exactly zero non-test hits, so every tenant's table was empty by
// construction, not by accident. The concrete cost: a workspace created in a
// client tenant on 2026-07-23 has no attributable record of who created it,
// even though POST /workspaces is AdminAuth-gated (so the caller definitely
// held a privileged credential — we just never wrote down WHICH one).
//
// This file closes that hole for the security-relevant lifecycle verbs:
// workspace create/delete and workspace auth-token mint/revoke.
//
// # Contract
//
// Rows are HMAC-chained per (workspace_id, agent_id). That grouping is not
// arbitrary: the verifier (verifyAuditChain) groups by agent_id, and the read
// handler always scopes its query to a single workspace_id — so a chain that
// spanned workspaces would present as a chain BREAK through the only surface
// that reads it. Chaining per (workspace_id, agent_id) is the only grouping
// that the shipped verifier can actually verify.
//
// agent_id carries the ACTOR (see auditActor). For an agent-execution event
// that is the agent; for an admin lifecycle event it is the credential class
// that authenticated the call ("admin-token", "org-token:<prefix>",
// "session:<workos-user-id>", …). This is the whole point of the exercise: it
// is the field that answers "who did this".
//
// # Failure policy
//
// Writes are best-effort with respect to the user's request: the ledger append
// happens AFTER the audited mutation has durably committed, so failing the
// request would report a false negative for work that already happened.
// Best-effort must never mean SILENT, so every failure path emits a
// "SECURITY/AUDIT" log line — the same loud marker getAuditHMACKey uses for an
// unset salt.
//
// # Unsigned rows
//
// When AUDIT_LEDGER_SALT is unset there is no key, so no HMAC can be computed.
// We still write the row — losing the RECORD is strictly worse than losing
// tamper-evidence — but stamp its hmac with the unsignedHMACPrefix so the
// degradation is self-declaring in the data rather than inferred from a config
// file. verifyAuditChain refuses to report such a chain as "verified".

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Operation values written by this package. Stable wire values — they are
// persisted, queried by operators, and covered by the HMAC. Extend by
// appending; never rename.
const (
	AuditOpWorkspaceCreate = "workspace.create"
	AuditOpWorkspaceDelete = "workspace.delete"
	AuditOpWorkspacePurge  = "workspace.purge"
	AuditOpTokenMint       = "auth_token.mint"
	AuditOpTokenRevoke     = "auth_token.revoke"
)

// unsignedHMACPrefix marks a row that was written while AUDIT_LEDGER_SALT was
// unset. Such a row is a genuine audit record but is NOT tamper-evident, and
// the verifier must never count it as verified.
const unsignedHMACPrefix = "unsigned:"

// auditActorUnknown is used when no credential marker reached the handler at
// all. It should never appear on an AdminAuth-gated route; if it does, that is
// a wiring bug worth seeing in the data.
const auditActorUnknown = "unknown"

// AuditEntry is the caller-facing description of one lifecycle event.
// WorkspaceID is the ANCHOR the row is filed under (audit_events.workspace_id
// is a NOT NULL FK), which is not always the subject — see auditDeleteAnchor.
type AuditEntry struct {
	WorkspaceID        string // anchor workspace (FK target); required
	Actor              string // → agent_id; who did it
	SessionID          string // → session_id; correlation id, may be empty
	Operation          string // → operation; one of the AuditOp* constants
	RiskFlag           bool
	HumanOversightFlag bool
	Details            map[string]any // → details; subject id, name, client ip, …
}

// auditActor resolves the caller identity from the gin context into the string
// persisted as agent_id.
//
// The middleware (AdminAuth / WorkspaceAuth) already distinguishes every
// credential class it accepts and stashes the discriminator in the context;
// nothing was ever reading it for audit purposes. Order is most-specific
// first: a named org token or a CP-verified human session identifies an actual
// principal, while caller_credential_class only identifies the credential
// KIND (the best available answer for a shared bootstrap ADMIN_TOKEN).
func auditActor(c *gin.Context) string {
	if c == nil {
		return auditActorUnknown
	}
	if v, ok := c.Get("org_token_prefix"); ok {
		if s, ok := v.(string); ok && s != "" {
			return "org-token:" + s
		}
	}
	if v, ok := c.Get("cp_session_user_id"); ok {
		if s, ok := v.(string); ok && s != "" {
			return "session:" + s
		}
	}
	if v, ok := c.Get("cp_session_actor"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if s := c.GetString("caller_credential_class"); s != "" {
		return s
	}
	return auditActorUnknown
}

// auditIsHuman reports whether the caller authenticated as a CP-verified
// human session (as opposed to any machine credential). Feeds
// human_oversight_flag, which is otherwise dead weight in the schema.
func auditIsHuman(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if v, ok := c.Get("cp_session_actor"); ok {
		if s, ok := v.(string); ok && s != "" {
			return true
		}
	}
	return false
}

// auditCorrelationID returns an upstream request id for session_id, so a
// ledger row can be joined against request logs. Empty when absent — the
// column is NOT NULL but the empty string is a valid value.
func auditCorrelationID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, h := range []string{"X-Request-Id", "X-Correlation-Id"} {
		if v := c.GetHeader(h); v != "" {
			if len(v) > 200 {
				v = v[:200]
			}
			return v
		}
	}
	return ""
}

// auditEntryFromGin builds an AuditEntry with the actor/session/human fields
// already resolved from the request, so call sites stay one-liners.
func auditEntryFromGin(c *gin.Context, workspaceID, operation string, riskFlag bool, details map[string]any) AuditEntry {
	if details == nil {
		details = map[string]any{}
	}
	if c != nil {
		if _, taken := details["client_ip"]; !taken {
			if ip := c.ClientIP(); ip != "" {
				details["client_ip"] = ip
			}
		}
	}
	return AuditEntry{
		WorkspaceID:        workspaceID,
		Actor:              auditActor(c),
		SessionID:          auditCorrelationID(c),
		Operation:          operation,
		RiskFlag:           riskFlag,
		HumanOversightFlag: auditIsHuman(c),
		Details:            details,
	}
}

// RecordAuditEvent appends one HMAC-chained row to audit_events.
//
// Never returns an error and never panics: it runs after the audited mutation
// has committed, so the caller has nothing useful left to do with a failure.
// Every failure is logged with the SECURITY/AUDIT marker so "the ledger stopped
// recording" is visible in operator dashboards instead of silent.
func RecordAuditEvent(ctx context.Context, database *sql.DB, e AuditEntry) {
	if database == nil {
		log.Printf("SECURITY/AUDIT: audit_events append skipped for operation=%q — no database handle", e.Operation)
		return
	}
	if e.WorkspaceID == "" {
		log.Printf("SECURITY/AUDIT: audit_events append skipped for operation=%q — no anchor workspace id", e.Operation)
		return
	}
	if e.Actor == "" {
		e.Actor = auditActorUnknown
	}

	var detailsJSON *string
	if len(e.Details) > 0 {
		// json.Marshal over a map emits compact, key-sorted JSON — the same
		// canonical form the HMAC covers. Stored in a TEXT column so the bytes
		// read back are byte-identical to the bytes signed (JSONB would
		// re-normalise them and every row would verify as tampered).
		b, err := json.Marshal(e.Details)
		if err != nil {
			log.Printf("SECURITY/AUDIT: audit_events details marshal failed for operation=%q: %v (recording event without details)", e.Operation, err)
		} else {
			s := string(b)
			detailsJSON = &s
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("SECURITY/AUDIT: audit_events append FAILED (begin) operation=%q workspace=%s actor=%s: %v",
			e.Operation, e.WorkspaceID, e.Actor, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Serialise appends to this chain. Two concurrent writers reading the same
	// tail would both link to it and produce a fork that the verifier reports
	// as a chain break — a self-inflicted false "tampered". The lock is
	// transaction-scoped and released by commit/rollback.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		e.WorkspaceID, e.Actor); err != nil {
		log.Printf("SECURITY/AUDIT: audit_events append FAILED (lock) operation=%q workspace=%s actor=%s: %v",
			e.Operation, e.WorkspaceID, e.Actor, err)
		return
	}

	// Read the chain tail. Ordering MUST mirror the read handler's
	// "ORDER BY timestamp ASC, id ASC", reversed — otherwise the writer links
	// to a different predecessor than the verifier walks.
	var prevHMAC sql.NullString
	var prevTS sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT hmac, timestamp
		FROM audit_events
		WHERE workspace_id = $1 AND agent_id = $2
		ORDER BY timestamp DESC, id DESC
		LIMIT 1
	`, e.WorkspaceID, e.Actor).Scan(&prevHMAC, &prevTS)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("SECURITY/AUDIT: audit_events append FAILED (chain tail) operation=%q workspace=%s actor=%s: %v",
			e.Operation, e.WorkspaceID, e.Actor, err)
		return
	}

	ts := auditNow()
	// Guarantee strict monotonicity within a chain. If two appends land in the
	// same microsecond, "ORDER BY timestamp, id" could order them against
	// insertion order and break the chain. Bumping by 1µs costs nothing and
	// cannot change the HMAC (the canonical form truncates to seconds).
	if prevTS.Valid && !ts.After(prevTS.Time) {
		ts = prevTS.Time.Add(time.Microsecond)
	}

	row := &auditEventRow{
		ID:                 uuid.NewString(),
		Timestamp:          ts,
		AgentID:            e.Actor,
		SessionID:          e.SessionID,
		Operation:          e.Operation,
		HumanOversightFlag: e.HumanOversightFlag,
		RiskFlag:           e.RiskFlag,
		Details:            detailsJSON,
		WorkspaceID:        e.WorkspaceID,
	}
	if prevHMAC.Valid {
		p := prevHMAC.String
		row.PrevHMAC = &p
	}

	row.HMAC = signAuditRow(row)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, timestamp, agent_id, session_id, operation,
			input_hash, output_hash, model_used,
			human_oversight_flag, risk_flag, prev_hmac, hmac, workspace_id, details
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`,
		row.ID, row.Timestamp, row.AgentID, row.SessionID, row.Operation,
		row.InputHash, row.OutputHash, row.ModelUsed,
		row.HumanOversightFlag, row.RiskFlag, row.PrevHMAC, row.HMAC, row.WorkspaceID, row.Details,
	); err != nil {
		log.Printf("SECURITY/AUDIT: audit_events append FAILED (insert) operation=%q workspace=%s actor=%s: %v",
			row.Operation, row.WorkspaceID, row.AgentID, err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("SECURITY/AUDIT: audit_events append FAILED (commit) operation=%q workspace=%s actor=%s: %v",
			row.Operation, row.WorkspaceID, row.AgentID, err)
		return
	}
	committed = true
}

// auditNow is a var so tests can pin time.
var auditNow = func() time.Time { return time.Now().UTC() }

// signAuditRow returns the value stored in the hmac column.
//
// With a key configured this is computeAuditHMAC — byte-identical to what the
// verifier recomputes on read. With no key (AUDIT_LEDGER_SALT unset) it is an
// UNKEYED digest carrying unsignedHMACPrefix: the event is still recorded, but
// the row itself declares that it is not tamper-evident, so an operator who
// later configures a salt sees "unsigned_events_present" rather than a
// misleading "tampered" verdict on rows that were never signed.
func signAuditRow(ev *auditEventRow) string {
	key := getAuditHMACKey()
	if key == nil {
		log.Printf("SECURITY/AUDIT: recording audit event %q for workspace %s with NO signature — "+
			"AUDIT_LEDGER_SALT is unset, so this row is auditable but NOT tamper-evident",
			ev.Operation, ev.WorkspaceID)
		sum := sha256.Sum256([]byte(auditCanonicalPayload(ev)))
		return unsignedHMACPrefix + hex.EncodeToString(sum[:])
	}
	return computeAuditHMAC(key, ev)
}

// auditIsUnsigned reports whether a stored hmac was written without a key.
func auditIsUnsigned(storedHMAC string) bool {
	return strings.HasPrefix(storedHMAC, unsignedHMACPrefix)
}

// auditTokenPrefix returns the non-secret leading characters of a freshly
// minted bearer, enough to correlate a ledger row with the token list UI and
// never enough to authenticate. A plaintext token must NEVER be written to the
// ledger: audit rows are readable by anyone who can read the workspace's audit
// trail, which is a strictly wider set than the one caller who received the
// token. Returns "" for short/empty input rather than leaking a whole token.
func auditTokenPrefix(plaintext string) string {
	const n = 8
	if len(plaintext) <= n {
		return ""
	}
	return plaintext[:n]
}

// auditAnyUnsigned reports whether any row in the result set is unsigned.
func auditAnyUnsigned(events []auditEventRow) bool {
	for i := range events {
		if auditIsUnsigned(events[i].HMAC) {
			return true
		}
	}
	return false
}

// auditDeleteAnchor picks the workspace a delete/purge event is filed under.
//
// audit_events.workspace_id is `NOT NULL REFERENCES workspaces(id) ON DELETE
// CASCADE`, and the hard-purge path additionally runs an explicit
// `DELETE FROM audit_events WHERE workspace_id = ANY(...)`. Filing a deletion
// under the workspace being deleted therefore erases the evidence of the
// deletion — the one event a tamper-evident log most needs to keep. Anchoring
// at the PARENT (which is never in the purge set: the purge set is the target
// plus its descendants) survives both paths, and a child's removal belongs in
// its parent's trail anyway.
//
// A root workspace has no parent and falls back to itself. Its purge still
// erases its own delete event — see the PR description; closing that requires
// an operator decision about the FK (relax to ON DELETE SET NULL, or ship the
// ledger off-box).
func auditDeleteAnchor(parentID, targetID string) string {
	if parentID != "" {
		return parentID
	}
	return targetID
}

// auditParentID looks up a workspace's parent, returning "" when it has none
// or the lookup fails. Only used to choose an anchor, so a failure degrades to
// "file it under the target" rather than dropping the event.
func auditParentID(ctx context.Context, database *sql.DB, id string) string {
	if database == nil || id == "" {
		return ""
	}
	var parent sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT parent_id FROM workspaces WHERE id = $1`, id).Scan(&parent); err != nil {
		return ""
	}
	if parent.Valid {
		return parent.String
	}
	return ""
}
