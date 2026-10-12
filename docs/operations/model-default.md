# Platform model default

The platform default is `minimax/MiniMax-M3` (MiniMax M3). The slash form
selects Molecule's metered proxy. The upstream model ID is `MiniMax-M3`.

For managed installations, `MOLECULE_LLM_DEFAULT_MODEL` in Infisical at
`/shared/controlplane/llm` is authoritative. The control plane loads it at boot
and returns it through `/cp/tenants/config`. Set and validate staging first,
then apply the same selector to production and restart the control plane on
its existing approved image. A configuration restart does not promote a new
application image or change the production release freeze.

Fresh concierges inherit this selector. Existing platform-managed concierges
reconcile it on their next provision; BYOK concierges retain their chosen model.
This setting does not bulk-edit worker workspace model secrets. A worker's
explicit model takes precedence over its template default. Claude Code,
Hermes, and OpenClaw templates default to platform M3; Codex retains its native
model because its Responses API transport has a separate compatibility contract.

For self-hosted installations, set `MOLECULE_LLM_DEFAULT_MODEL` to a model
registered for the chosen runtime and supply its credential. For example,
Claude Code uses bare `MiniMax-M3` with `MINIMAX_API_KEY` for direct MiniMax
access. A platform slash ID requires the platform proxy configuration. Empty
self-hosted configuration still waits for onboarding; changing the fallback
does not configure credentials.

When changing the default, align the core and control-plane unset-value
fallbacks, runtime catalog, template menus, and E2E defaults. Keep older model
IDs selectable. Validate provider routing, template credential requirements,
override precedence, and a real streamed tool turn through staging before
applying the production selector. Restore the previous selector and restart
the control plane to roll back; running workspaces keep their persisted model
until they are reconfigured or reprovisioned.
