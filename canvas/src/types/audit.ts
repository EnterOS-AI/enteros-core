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
}

/** Offset-paginated response envelope from GET /workspaces/:id/audit. */
export interface AuditResponse {
  events: AuditEvent[];
  total: number;
  /** null means the server could not verify this page or filtered subset. */
  chain_valid: boolean | null;
}
