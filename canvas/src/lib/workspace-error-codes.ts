/** Workspace lifecycle error codes — the TS mirror of the raw `code` strings
 *  the workspace-server emits (no Go const block exists yet; each code below
 *  names its exact Go emit site — filing the Go-side const block is tracked
 *  as a follow-up, see the self-host onboarding design SSOT §5.2).
 *
 *  Two distinct wire channels carry these codes:
 *
 *  1. 422-BODY codes — returned synchronously in the JSON body of a
 *     rejected create/model mutation as `{"code": "...", "error": "..."}`
 *     (HTTP 422 Unprocessable Entity).
 *  2. SOCKET-EXTRA codes — carried in the `WORKSPACE_PROVISION_FAILED`
 *     websocket event payload (the provisionAbort `Extra` map:
 *     `payload.code` / `payload.error`), and echoed into the workspace's
 *     `last_sample_error` text for poll-path consumers.
 *
 *  Kept in a leaf module (zero imports) per the `workspace-kind.ts` pattern.
 *
 *  DRIFT CONSEQUENCE: if a Go emit site renames/adds a code and this mirror
 *  is not updated in the same change, canvas error mapping silently falls
 *  through to the generic copy — users see "couldn't set up" instead of the
 *  actionable per-code message. Update this file whenever the emit sites
 *  below change. */
export const WORKSPACE_ERROR_CODES = {
  // ── 422-body codes (channel 1) ────────────────────────────────────────────
  /** Create refused: no model given and the platform provides no default.
   *  Emitted by handlers/workspace.go:525 (CreateWorkspace). */
  ModelRequired: "MODEL_REQUIRED",
  /** The (runtime, model) pair is not in the provider registry for that
   *  runtime. Emitted by handlers/workspace.go:572 (CreateWorkspace) AND
   *  handlers/secrets.go:954 (SetModel — PUT /workspaces/:id/model). */
  UnregisteredModelForRuntime: "UNREGISTERED_MODEL_FOR_RUNTIME",
  /** Runtime rejected: external workspaces off the external-like set
   *  (handlers/workspace.go:469) or an unknown runtime entirely
   *  (handlers/workspace.go:479). */
  RuntimeUnsupported: "RUNTIME_UNSUPPORTED",
  /** BYOK-routed model has no usable credential at create time.
   *  Emitted by handlers/workspace.go:618 (CreateWorkspace, 422 body).
   *  ALSO emitted on channel 2 at provision time — see below. */
  MissingByokCredential: "MISSING_BYOK_CREDENTIAL",
  /** Runtime could not be resolved from the requested template; the server
   *  refuses to silently provision the default runtime (controlplane#188).
   *  Emitted by handlers/workspace.go:440. */
  RuntimeUnresolved: "RUNTIME_UNRESOLVED",
  /** The model's derived provider is missing from the runtime's registry.
   *  Emitted by handlers/workspace.go:598. */
  DerivedProviderNotInRegistry: "DERIVED_PROVIDER_NOT_IN_REGISTRY",

  // ── WORKSPACE_PROVISION_FAILED socket-extra codes (channel 2) ─────────────
  // MissingByokCredential (above) is ALSO a channel-2 code: emitted by
  // handlers/workspace_provision_shared.go:230 when the provision-time
  // credential re-check fails (molecule-core#1994).
  /** Platform-routed workspace but the CP proxy env is absent — the
   *  platform-managed arm does not exist on this stack (self-host).
   *  Emitted by handlers/workspace_provision_shared.go:242
   *  (molecule-core#2162). */
  MissingPlatformProxy: "MISSING_PLATFORM_PROXY",
  /** No resolved model at provision time; the server refuses the runtime's
   *  opaque default. Emitted by handlers/workspace_provision_shared.go:295
   *  (core#2594). */
  MissingModel: "MISSING_MODEL",
} as const;

/** Union of all known workspace error-code strings. */
export type WorkspaceErrorCode =
  (typeof WORKSPACE_ERROR_CODES)[keyof typeof WORKSPACE_ERROR_CODES];
