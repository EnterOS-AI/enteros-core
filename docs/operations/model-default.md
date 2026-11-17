# Platform model default

The platform default is `google/gemini-4-carbon` (Gemini 4 Carbon). The slash
form selects Molecule's metered proxy, which serves `google/*` through Google AI
Studio with the platform key. The upstream model ID is `gemini-4-carbon`. The
default before it was `minimax/MiniMax-M3`.

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
Hermes, and OpenClaw templates default to platform Carbon; Codex retains its
native model because its Responses API transport has a separate compatibility
contract (the proxy's Responses surface serves only OpenAI models).

End-to-end lanes never run on the default. They use
`google/gemini-3.1-flash-lite`, the cheapest Gemini model that serves a tool
call: `E2E_LLM_UPSTREAM: gemini` in the e2e workflows gives the throwaway control
plane the platform Gemini key and that model as its `MOLECULE_LLM_DEFAULT_MODEL`,
pins the bundled base templates' default to it, and every harness that picks a
platform model (`tests/e2e/lib/model_slug.sh`, `tests/harness`, the canvas
staging setup) falls back to it. `ci-llm` and `vendor` stay selectable.

For self-hosted installations, set `MOLECULE_LLM_DEFAULT_MODEL` to a model
registered for the chosen runtime and supply its credential. For example,
Hermes uses `gemini/gemini-4-carbon` with `GEMINI_API_KEY` for direct Gemini
access (Claude Code has no direct Gemini route; it reaches Gemini only through
the platform proxy), and Claude Code uses bare `MiniMax-M3` with
`MINIMAX_API_KEY` for direct MiniMax access. A platform slash ID requires the
platform proxy configuration. Empty
self-hosted configuration still waits for onboarding; changing the fallback
does not configure credentials.

When changing the default, align the core and control-plane unset-value
fallbacks, runtime catalog, template menus, and E2E defaults. Keep older model
IDs selectable. Validate provider routing, template credential requirements,
override precedence, and a real streamed tool turn through staging before
applying the production selector. Restore the previous selector and restart
the control plane to roll back; running workspaces keep their persisted model
until they are reconfigured or reprovisioned.
