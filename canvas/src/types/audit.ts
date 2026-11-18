/** Stored audit event returned by GET /workspaces/:id/audit. */
export interface AuditEvent {
  id: string;
  timestamp: string;
  agent_id: string;
  session_id: string;
  operation: string;
  input_hash: string | null;
  output_hash: string | null;
  model_used: string | null;
  human_oversight_flag: boolean;
  risk_flag: boolean;
  prev_hmac: string | null;
  hmac: string;
  workspace_id: string;
  /**
   * Compact JSON naming the event's subject (which workspace was deleted,
   * which token id was revoked, client IP). Present on lifecycle events
   * written by the server; null on agent-execution events and on rows that
   * predate the column. Covered by the HMAC — do not re-serialise it before
   * verifying.
   */
  details: string | null;
}

/** Verification verdict accompanying an audit page. */
export type AuditChainVerification =
  | 'verified'
  | 'tampered'
  | 'unavailable_partial_query'
  | 'disabled_no_salt'
  | 'unsigned_events_present';

/** Offset-paginated response envelope from GET /workspaces/:id/audit. */
export interface AuditResponse {
  events: AuditEvent[];
  total: number;
  /** null means the server could not verify this page or filtered subset. */
  chain_valid: boolean | null;
  /**
   * Why chain_valid has the value it has. Clients must not collapse
   * `disabled_no_salt` / `unsigned_events_present` into "fine" — both mean
   * the ledger is not currently tamper-evident.
   */
  chain_verification: AuditChainVerification;
}
