# Setting plugin config from a workspace template (`plugins[].config`)

How a workspace template supplies values for a plugin's declared settings — and
the one identity mistake that makes the whole thing fail without an error.

The declaration side is [`authoring-configuration.md`](authoring-configuration.md).
Core writer: `workspace-server/internal/handlers/plugin_settings_delivery.go`.

## The two forms of a `plugins:` entry

A template's `config.yaml` may list plugins in either form. Both are valid and
both keep parsing byte-identically:

```yaml
plugins:
  # bare string — install only, no settings
  - gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0

  # object — install AND settings
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      poll_seconds: 5
      schedules:
        - name: morning-standup
          cron: "0 9 * * 1-5"
          timezone: America/Vancouver
          prompt: Summarise what changed overnight.
```

**Only the object form delivers anything.** A bare string produces no settings
file at all — core writes no `plugin-settings/` entry for it. If you add a
`config:` block and nothing changes, first check you actually converted the
entry to the object form; a `config:` key on a string entry is not a thing the
parser can see.

Removal markers (`!name`, `-name`) carry no settings and are skipped even in the
object form.

> **An entry object without `source` takes down the whole block.** This is the
> one failure here that is *not* per-entry. It fails at YAML-parse time, so
> `renderPluginSettingsFiles` returns an error and **no plugin in the template
> gets a settings file** — including entries that were perfectly well-formed.
> Executed:
>
> ```
> plugins:
>   - config: {poll_seconds: 5}                      # ← no source:
>   - source: gitea://…/molecule-ai-plugin-scheduler#v0.2.0
>     config: {poll_seconds: 9}                      # ← well-formed sibling
>
> → err = "plugin entry object is missing `source`",  files = []
> ```
>
> The provision still succeeds — the error is logged and swallowed, because a
> broken plugins block must never block a create. So the symptom is that
> *every* plugin's settings quietly vanish at once. If several plugins stopped
> picking up config simultaneously, suspect a malformed sibling entry rather
> than each plugin.

## The two-identity trap — read this before anything else

**A plugin has two names, and the settings file uses the one you probably do not
expect.**

| Identity | Scheduler's value | What it keys |
| --- | --- | --- |
| **install name** — the checkout directory, derived from **the `source:` string** by `PluginNameFromSource` (for the scheduler, the repo — but see the derivation table below, it is not always the repo) | `molecule-ai-plugin-scheduler` | **the settings file** — core writes `plugin-settings/<this>.json`, the runtime reads it with the same key |
| **manifest name** — the manifest's own `name:` | `molecule-scheduler` | daemon provenance for `kind: channel` and `kind: trigger` |

Core derives the filename from the **source string you wrote in the template**,
never from the manifest — it has no manifest at provision time. Executed against
the real scheduler source:

```
SchedulerPluginSource        = "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"
PluginNameFromSource(source) = "molecule-ai-plugin-scheduler"
manifest name:               = "molecule-scheduler"

renders → plugin-settings/molecule-ai-plugin-scheduler.json
```

### Why this is dangerous rather than merely confusing

Getting it wrong **writes a file nobody reads, and nothing tells you.** The
runtime treats a missing settings file as a clean no-op — it falls back to the
manifest's declared defaults and the plugin loads normally. There is no error,
no warning at the template layer, and the workspace looks healthy. The only
symptom is that your value silently did not apply.

This bites in two directions:

- **Hand-writing the settings file** (debugging, a runbook step, a migration
  script) under the manifest name. Always
  `/configs/plugin-settings/molecule-ai-plugin-scheduler.json`.
- **Keying anything on the manifest name in tooling** that generates or
  inspects template config.

Note the plugin's own `plugin.yaml` comment currently names
`/configs/plugin-settings/molecule-scheduler.json`. That comment is wrong; the
delivered filename is what the code produces, and the code uses the install
name.

### The rule, and how to derive the name

**The settings filename comes from your `source:` string, never from the plugin's
name.** It is not simply "the repo" — the derivation differs per scheme, and
`gitea://` and `github://` disagree about subpaths. Executed against
`PluginNameFromSource`:

| `source:` | install name → `plugin-settings/<name>.json` |
| --- | --- |
| `gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0` | `molecule-ai-plugin-scheduler` (the repo) |
| `gitea://molecule-ai/repo/plugins/my-plugin#sha:…` | `my-plugin` — **the last subpath segment, not the repo** |
| `gitea://molecule-ai/repo/a/b/c` | `c` |
| `github://owner/repo#sha:…` | `repo` |
| `github://owner/repo/sub/dir` | `repo` — **github ignores the subpath**; gitea does not |
| `local://my-plugin` / bare `my-plugin` | `my-plugin` |

So a mono-repo `gitea://` source keys on its **subpath leaf**, while the same
shape under `github://` keys on the **repo**. If you migrate a plugin between the
two schemes, the settings filename can change underneath you and the settings
silently stop applying.

Do not compute this from memory. Derive it the way core does, or read the
filename off a provisioned workspace — and never off the manifest.

## What core does with your `config:` block

1. Parses the `plugins:` block. **A parse failure loses settings for every
   plugin in the template** (see the missing-`source` box above) — it is logged
   and swallowed, so provisioning still succeeds.
2. Past the parse, **validate and skip per entry**: one unusable entry never
   drops its siblings. Entries with no `config:` are skipped.
3. Derives the install name from `source`. A name that is not a bare path
   segment is refused (it would escape the settings directory).
4. Serialises the `config:` map to JSON. Not JSON-encodable, or over **64 KiB**
   for a single plugin → skipped with a log line.
5. Writes `plugin-settings/<install-name>.json` into the delivered bundle.

Entries are processed in sorted-source order so a re-provision produces
byte-identical output.

**Nothing in that list fails the provision.** A mistake in your `config:` block
surfaces as a log line and an absent setting, never as a failed create — which is
why these errors are easy to miss. If a value did not take, read the provision
logs for `plugin settings:`. Note the blast radius differs by step: step 1 loses
the settings for *every* plugin in the template, steps 2–4 lose only the offending
entry.

## Where the values land, and when

```
template config.yaml   plugins[].config
        ↓  core   renderPluginSettingsFiles
/configs/plugin-settings/<install-name>.json
        ↓  runtime  plugin_settings.resolve   (manifest defaults ← delivered)
daemon env  (${config:key} interpolated)  +  MOLECULE_PLUGIN_CONFIG_FILE
```

Settings live **outside** `/configs/plugins/<name>/` on purpose: the install
pipeline owns that directory and re-stages it (EIC does a literal `rm -rf`), so
anything written there dies on the next reconcile.

**Delivery is Create-path only.** Both call sites sit inside
`WorkspaceHandler.Create` — the local-template leg and the SaaS fetched-bytes
leg. Editing a template does **not** reach workspaces already provisioned from
it; they need to be provisioned again.

The live-edit path (**layer 6**) IS in `main` and is **on by default** since
core#5047: an operator override recorded through `PATCH .../plugin-settings`
is re-overlaid onto the delivered bytes on every (re-)provision, so the edit
survives on the box and not merely in the database. Operators can revert to
pure template delivery without a redeploy by setting the kill-switch
`MOLECULE_PLUGIN_SETTINGS_LAYERS` to `0`/`false`/`no`/`off`.

On SaaS, the template's real bytes arrive through the Gitea asset channel rather
than a local path, and both legs run. On key collision the **fetched** render
wins, because on SaaS the fetched bytes are the real template while the local
path may be a `<runtime>-default` fallback that never declared these plugins.

## Precedence

```
layer 1    plugin manifest    contributes.configuration.<key>.default
layer 2-5  delivered file     your plugins[].config
```

Delivered values win, shallow per key. You do not need to restate a plugin's
defaults — set only what you are changing.

An undeclared key is **kept** with a warning rather than dropped, so a typo'd
key name lands in the file and is simply never read by the plugin. Check your
key names against the plugin's `contributes.configuration.properties`.

## Checklist

- [ ] Entry is the **object** form (`source:` + `config:`), not a bare string.
- [ ] Key names match the plugin's declared `properties` — a typo is kept, not
      rejected.
- [ ] If hand-writing or inspecting the file, its name was derived from the
      **`source:` string** (`molecule-ai-plugin-scheduler.json`), not from the
      manifest (`molecule-scheduler.json`) — and check the derivation table if
      the source carries a subpath.
- [ ] The change is expected to reach only **newly provisioned** workspaces.
- [ ] Provision logs checked for `plugin settings:` skip lines if a value did
      not apply.

## Related

- [`authoring-configuration.md`](authoring-configuration.md) — declaring the keys
  on the plugin side, and when to mark one `sensitive`
- [`sources.md`](sources.md) — the `local` / `github` / `gitea` source schemes
  the install name is derived from
- `docs/runbooks/scheduler-plugin.md` — the scheduler's own config keys
