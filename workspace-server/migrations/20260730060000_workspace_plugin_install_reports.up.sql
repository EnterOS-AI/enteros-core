-- What the BOX said about installing its declared plugins, per workspace.
--
-- SDK contract: contracts/plugin-install-report/. The runtime already computed
-- this on every boot (molecule_runtime.plugin_sources.InstallReport) and then
-- printed it to a stdout nobody can read on a locked-down prod box. The BOOT_STEP
-- telemetry that would have carried it is concierge-gated ("so an ordinary,
-- non-concierge tenant boot doesn't spam the endpoint") and POST /boot-event is
-- BroadcastOnly — "presentation event, no structure_events row". So the one class
-- of box whose plugin install could silently produce nothing was also the one
-- class core could not see.
--
-- Measured 2026-07-30 (molecule-core#4953): every kind=workspace workspace on both
-- prod tenants had loaded_mcp_tools:[] and an empty schedule grid, and core could
-- not answer "did boot-install run, and what did it say" for any of them.
--
-- LATEST-PER-WORKSPACE, not a log. The question this answers is "what happened on
-- the last boot" — an append-only history would need a retention policy to avoid
-- becoming the biggest table in the tenant DB (one row per workspace per boot,
-- and a wedged workspace reboots repeatedly). A boot that fails REPEATEDLY
-- overwrites with the same content, which is the right shape: the symptom is the
-- current state, not its multiplicity. Provision/boot events remain in
-- structure_events for sequencing.
CREATE TABLE IF NOT EXISTS workspace_plugin_install_reports (
  workspace_id  UUID        PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,

  -- Whether MOLECULE_DECLARED_PLUGINS was non-empty at boot. FALSE separates
  -- "core never asked for a plugin" from "core asked and the box could not
  -- deliver" — two different bugs in two different repos, and previously
  -- indistinguishable from outside the box.
  declared      BOOLEAN     NOT NULL,

  -- Resolved live plugins directory the staging tree is swapped into.
  plugins_dir   TEXT        NOT NULL DEFAULT '',

  -- Sources that materialised into the STAGING tree. NOT the same as live —
  -- see swapped.
  installed     JSONB       NOT NULL DEFAULT '[]'::jsonb,
  -- Sources deliberately not fetched (unknown scheme, missing credential).
  skipped       JSONB       NOT NULL DEFAULT '[]'::jsonb,
  -- Sources that could not be fetched or staged. Non-empty ⇒ never promoted.
  failed        JSONB       NOT NULL DEFAULT '[]'::jsonb,

  -- THE load-bearing column. Whether the freshly built staging tree was
  -- atomically swapped into plugins_dir. A partial build is never promoted, so
  -- installed=[6 sources] with swapped=false means NOTHING went live — a state
  -- that was indistinguishable from success in every signal core had before this
  -- table existed. Liveness is `declared AND swapped`
  -- (molcontracts.PluginInstallReportOutcomeRule); reading `installed` alone is
  -- the mistake that rule exists to prevent, which is why no `live` column is
  -- stored here — a derived column would be one more place for the rule to drift.
  --
  -- `failed` is NOT part of liveness (corrected in core#4972; this comment still
  -- carried the retired `AND failed = []` form). The runtime PROMOTES partial
  -- builds on purpose — molecule_runtime/plugin_sources.py, "A failed source fails
  -- THAT SOURCE — not the whole tree" — so swapped=true with a non-empty `failed`
  -- means the successful sources ARE live. That case is `degraded`
  -- (molcontracts.PluginInstallReportDegradedRule), also derived on read, and it
  -- is why the fleet endpoint serves two arms rather than one. Comment-only fix:
  -- the DDL below is unchanged and this migration replays identically.
  swapped       BOOLEAN     NOT NULL,

  reported_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The fleet question — "which workspaces booted with no live plugins" — is the
-- one an operator asks first, and it is a scan over a predicate rather than a
-- lookup by id, so the primary key does not serve it.
CREATE INDEX IF NOT EXISTS workspace_plugin_install_reports_not_live
  ON workspace_plugin_install_reports(reported_at DESC)
  WHERE declared AND NOT swapped;
