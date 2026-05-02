# Upstream PR draft: Pluggable platform adapters for hermes-agent

**Status:** Draft — pre-submission review
**Target repo:** `NousResearch/hermes-agent`
**Owner:** Molecule AI (hongmingwang@moleculesai.app)
**Date drafted:** 2026-05-02

---

## Why this draft exists

Molecule needs to deliver A2A inbox messages to a hermes-hosted agent the same way Telegram messages reach it today — through `_handle_message`, with `set_busy_session_handler` semantics for mid-turn arrivals. Today this requires forking `gateway/run.py` because the platform adapter system is closed (`_create_adapter` is a hardcoded if/elif chain at lines 2424-2578).

But hermes already ships a working plugin discovery system for memory backends (`plugins/memory/__init__.py`). Extending the same pattern to platforms is a small, symmetric change — not novel architecture. This draft documents the proposed upstream PR before we open it, so we can iterate locally on tone, scope, and code shape.

---

## Proposed PR title

> Pluggable platform adapters via `plugins/platforms/` discovery

(Mirrors the existing `plugins/memory/` shape so the title alone signals "this is the same pattern, just for the other subsystem.")

---

## PR body

### Problem

Hermes ships 19 in-tree platform adapters (Telegram, Discord, WhatsApp, Slack, Signal, Mattermost, Matrix, Email, SMS, DingTalk, Feishu, WeCom variants, Weixin, BlueBubbles, QQBot, HomeAssistant, API server, Webhook). Each is wired by editing two files:

- `gateway/config.py:48-69` — append a `Platform` enum value
- `gateway/run.py:2424-2578` — append an `elif platform == Platform.X:` branch in `_create_adapter()`

For platforms with broad demand (Telegram, Slack, etc.) this is fine: the maintenance load lives upstream, every user benefits. For platforms with narrow but real demand — enterprise-internal channels (Rocket.Chat, RingCentral, Zulip), agent-to-agent inbox protocols (e.g. Molecule's A2A), niche regional platforms, or experimental transports — the only path today is forking `gateway/run.py`. Forks drift, defeat the purpose of an OSS gateway, and discourage contribution back upstream.

### Prior art (already in hermes)

The memory subsystem solved exactly this problem at `plugins/memory/__init__.py`:

1. **Two-tier discovery** — bundled providers in `plugins/memory/<name>/` plus user-installed providers in `$HERMES_HOME/plugins/<name>/`. Bundled wins on name collision.
2. **`register(ctx)` collector pattern** (`plugins/memory/__init__.py:264-305`) — a plugin's `__init__.py` exposes a `register(ctx)` function; `ctx` already supports `register_memory_provider`, `register_tool`, `register_hook`, `register_cli_command`.
3. **`plugin.yaml` manifest** for description and metadata.
4. **Config-driven activation** (`memory.provider: honcho` selects which provider loads).

Adding `register_platform_adapter` to the same collector and a `plugins/platforms/` discovery directory extends this pattern symmetrically.

### Proposal

**Three small changes:**

1. **New collector method** in `plugins/memory/__init__.py:_ProviderCollector` (or a new shared `plugins/_collector.py` if maintainers prefer cleaner separation):

   ```python
   def register_platform_adapter(self, name: str, adapter_class: type, requirements_check=None):
       """Register a platform adapter loadable as plugin.

       name: unique platform identifier (matches gateway.platforms.<name> in config)
       adapter_class: subclass of BasePlatformAdapter
       requirements_check: optional callable returning bool — same shape as
                          existing check_telegram_requirements() etc.
       """
       self.platform_adapters[name] = (adapter_class, requirements_check)
   ```

2. **New `plugins/platforms/__init__.py`** mirroring `plugins/memory/__init__.py` — `discover_platform_adapters()`, `load_platform_adapter(name)`, two-tier (bundled + `$HERMES_HOME/plugins/`) discovery.

3. **`_create_adapter()` fallback** at `gateway/run.py:2578` — after the in-tree if/elif chain returns None, attempt plugin lookup:

   ```python
   # Existing in-tree adapters checked first (precedence preserved).
   # If no match, fall through to plugin discovery.
   from plugins.platforms import load_platform_adapter
   plugin_entry = load_platform_adapter(platform.value)
   if plugin_entry:
       adapter_class, req_check = plugin_entry
       if req_check and not req_check():
           logger.warning(f"{platform.value}: plugin requirements not met")
           return None
       return adapter_class(config)
   return None
   ```

4. **`Platform` enum becomes open-set.** Today it's `Enum`; switch to a string-backed pattern that accepts unknown values (still validates against the union of in-tree + discovered plugins at config-load time):

   ```python
   # gateway/config.py — replace Enum with frozen dataclass + dynamic registry.
   # Keeps the in-tree values as module-level singletons for backward compat:
   # Platform.TELEGRAM still works as today.
   ```

   This is the only "shape change" in the PR. Backward compat is straightforward: every existing `Platform.TELEGRAM` reference continues to work because the module exports the same names.

### Backward compatibility

- All 19 in-tree adapters keep their hardcoded path in `_create_adapter()` (precedence: in-tree wins on name collision, exactly like memory plugins).
- Existing config files (`gateway.platforms.telegram.enabled: true`) continue to work unchanged.
- No new mandatory config keys.
- Plugin discovery only runs if the platform name doesn't match an in-tree value, so cold-start cost is zero for users who don't use plugins.
- Fork-then-add-platform users can migrate to plugins at their own pace; the in-tree path isn't deprecated.

### Test plan

- **Unit**: discovery scans both bundled and user dirs, respects precedence.
- **Unit**: `_create_adapter()` falls through to plugin lookup only when in-tree doesn't match.
- **Integration**: ship a minimal `plugins/platforms/example/` in-tree (read-only, returns canned messages) so CI exercises the full plugin code path. Same approach `plugins/memory/holographic/` takes today.
- **Manual**: Molecule will publish `hermes-platform-molecule-a2a` as the first external consumer once this lands.

### Documentation

- Extend `CONTRIBUTING.md`'s "Should it be a Skill or a Tool?" section with "Should it be a Platform Plugin or an in-tree Platform?" — same shape, same decision tree.
- Add `plugins/platforms/README.md` mirroring `plugins/memory/`'s convention.

### Out of scope (intentionally)

- **Setuptools `entry_points`** — could be added later as a third discovery tier (after bundled + `$HERMES_HOME/plugins/`). Skipping for v1 because the directory-based discovery already covers the demand and matches the memory pattern. Adding entry_points is a non-breaking extension.
- **Hot-reload** — plugins discovered at gateway boot, no live re-scan. Matches memory plugins.
- **Sandboxing** — plugins run with full hermes process privileges. Same trust model as memory plugins; documented in the new README.

### Reference consumer

Molecule AI will ship `hermes-platform-molecule-a2a` as the first external consumer. Use case: deliver agent-to-agent inbox messages (from peer agents authenticated at the platform layer, not the Telegram-user level) into the same `_handle_message` dispatch Telegram uses, with `internal=True` events to bypass user-auth. Expected timeline: within 2 weeks of merge.

---

## Open questions for upstream maintainers

Per `CONTRIBUTING.md`, the right channel for design proposals is
**GitHub Discussions**, not Discord (Discord is for "questions,
showcasing projects, and sharing skills" — Discussions is the
documented channel for "design proposals and architecture discussions").

Open a Discussion at `NousResearch/hermes-agent/discussions` titled
"RFC: pluggable platform adapters via `plugins/platforms/`" with the
problem + proposal + open questions before filing the PR. This gives
maintainers space to weigh in on shape before code is in flight.

Open questions to put in the Discussion:

1. **Preferred naming.** `register_platform_adapter` vs `register_platform` vs `register_channel`. Consistency with memory's `register_memory_provider` argues for the long form.
2. **Enum vs string.** Is the maintainer team open to making `Platform` open-set? If not, fallback design: keep enum, add a single `Platform.PLUGIN` sentinel + a `plugin_name` field on `PlatformConfig`. Slightly uglier but smaller blast radius.
3. **Testing**: `plugins/platforms/example/` checked into the repo, or test-fixtures-only? Memory plugins are real (mem0, honcho, supermemory bundled), so a real example seems consistent.
4. **Discovery ordering**: confirm the user wants bundled-wins precedence (matches memory) vs user-can-override-bundled (would let downstream patch a buggy in-tree adapter without forking). Current memory pattern is bundled-wins; we'll match it unless told otherwise.

---

## Effort estimate

- **Code change**: ~150 LOC across `plugins/platforms/__init__.py` (new), `gateway/config.py` (Platform refactor), `gateway/run.py` (10-line fallback in `_create_adapter`), tests (~50 LOC).
- **Docs**: ~80 LOC across `CONTRIBUTING.md` extension and new `plugins/platforms/README.md`.
- **Review cycle**: depends on maintainer responsiveness. Memory plugin system shipped in v0.5–0.7 era; platform plugin system would land for v0.11 if accepted.

---

## After this PR lands (Molecule-side follow-up)

1. Publish `hermes-platform-molecule-a2a` (PyPI + `~/.hermes/plugins/molecule-a2a/`).
2. Bump our hermes workspace template to declare `plugins.platforms.molecule_a2a.enabled: true`.
3. Remove the polling shim from `molecule-ai-workspace-template-hermes/adapter.py` once the plugin path is verified end-to-end.

---

## Status checklist (for our own tracking)

Per user's gating: "if the plugin works locally in our docker setup
and e2e testing works, yes [submit]". Validation prerequisites:

- [ ] Build a working `plugins/platforms/molecule_a2a/` plugin against
      a forked hermes-agent with the proposed change applied
- [ ] Bake the forked hermes + plugin into a local copy of our
      `molecule-ai-workspace-template-hermes` Docker image
- [ ] E2E: boot the local image, send A2A messages from a peer agent,
      observe `_handle_message` dispatch + reply through A2A queue
- [ ] Confirm `Platform` enum refactor doesn't break downstream — grep
      for `Platform.X` usages across hermes
- [ ] Confirm `$HERMES_HOME` is the right user-plugin root for
      platforms (matches memory convention)
- [ ] Open a GitHub Discussion at
      `NousResearch/hermes-agent/discussions` titled
      "RFC: pluggable platform adapters via plugins/platforms/" with
      design + open questions; wait for maintainer feedback
- [ ] Branch name: `feat/pluggable-platform-adapters` per
      CONTRIBUTING.md branch convention
- [ ] Commit prefix: `feat(gateway): pluggable platform adapters
      via plugins/platforms/` per Conventional Commits + scope `gateway`
- [ ] PR description covers what/why + how-to-test + platforms tested,
      per CONTRIBUTING.md PR-description requirements
- [ ] Open PR against `NousResearch/hermes-agent` main once Discussion
      lands consensus
- [ ] Track PR; bump cadence weekly; if stalled past 4 weeks, propose
      fork-and-bundle as fallback for our hermes template image
