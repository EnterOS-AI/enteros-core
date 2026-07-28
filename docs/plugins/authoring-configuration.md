# Declaring plugin configuration (`contributes.configuration`)

How a plugin declares the settings an install may set, and when to mark a key
`sensitive`.

Contract: `contracts/plugin-manifest/plugin-manifest.schema.json` in
`molecule-ai-sdk` (sdk#176). Runtime half:
`molecule_runtime/plugin_settings.py`. Core writer:
`workspace-server/internal/handlers/plugin_settings_delivery.go`. Setting the
values from a template is the other side of this seam —
[`template-plugin-config.md`](template-plugin-config.md).

## What the declaration is for

`contributes.configuration` declares **which keys an install may set**. It does
not carry the values. The values are delivered out of band as
`/configs/plugin-settings/<install-name>.json`, and the runtime merges them over
your declared defaults at daemon-discovery time.

Three consumers read the declaration:

- an **ecosystem developer** reads `default` to run the plugin with no API call;
- an **operator** reads `description` / `enum` to review a value and reuse it
  across workspaces;
- the **platform UI** renders the plugin tab from `type` / `enum` / `default`,
  and masks the field when `sensitive`.

## Shape

```yaml
contributes:
  configuration:
    title: Scheduler                  # section heading in the plugin tab
    description: Tuning for the …     # section help text
    properties:
      <key>:
        type: string|integer|number|boolean|array|object
        default: <layer-1 value>
        description: <human-facing help>
        enum: [<closed value set>]    # optional; UI renders a select
        sensitive: true|false         # default false
        required: true|false          # default false, ADVISORY ONLY
```

**Key names must match `^[A-Za-z0-9_.-]+$`.** That is the runtime's reference
grammar (`_CONFIG_REF` in `plugin_settings.py`). A key outside it can be written
down but could never be referenced as `${config:<key>}`, so the canonical branch
of the schema rejects it.

## Consuming the values

Two ways, and you can use either.

**1. Interpolate into your own daemon env** — the zero-code-change path. Any
`${config:<key>}` in a `daemons[].env` (or `mcpServers[].env`) value is
substituted at discovery time:

```yaml
contributes:
  daemons:
    - name: scheduler
      command: python
      args: [scheduler.py]
      env:
        MOLECULE_TRIGGER_POLL_SECONDS: "${config:poll_seconds}"
```

An existing plugin that already reads an env var keeps working and simply
sources that var from settings.

**2. Read the file directly** — the runtime sets `MOLECULE_PLUGIN_CONFIG_FILE`
in your daemon's environment, pointing at your resolved settings file. Use this
for structured values (arrays, objects) that do not flatten into an env string.

> **Check before you name an env var.** `spec.env` *overlays* the process env at
> spawn, so declaring a var an operator already sets would silently override
> them. Grep core, the runtime, your plugin and the template repos for the name
> first.

## Precedence, and why `default` lives in the manifest

```
layer 1  contributes.configuration.<key>.default   ← your manifest, ON THE BOX
layer 2-5  delivered plugin-settings/<name>.json   ← template / org / operator
```

Resolution is a shallow per-key override: delivered values win, key by key.

Layer 1 cannot be rendered by core, and this is structural rather than an
oversight: core holds no plugin manifest at provision time, because plugins
install post-online via the reconcile — strictly *after* the settings file
ships. The runtime supplies layer 1. The practical consequence for you: **an
install that sets nothing must keep working on your declared defaults alone.**

## Failure policy: bad keys are dropped, the plugin is kept

This is decided and enforced, and it shapes how you should declare:

- A settings file that is absent, unreadable, malformed, not a JSON object, or
  over 256 KiB degrades to `{}` — your declared defaults apply.
- An unresolvable `${config:foo}` renders as the **empty string**, never the
  literal `${config:foo}`.
- A delivered key you did **not** declare is **kept**, with a warning. It is not
  dropped: core is the side that validates against the declaration, and
  discarding it here would make the two sides disagree about what shipped.
- `required: true` is **advisory only**. An unset required key resolves to empty
  like any other unresolved reference; it does **not** fail the install.

Design to that: a setting must never be able to take the plugin down. Handle the
empty string, clamp out-of-range numbers, and fall back rather than raising —
the way the scheduler clamps `poll_seconds` to a 1s floor and falls back to 30.

## When to mark a key `sensitive`

Mark `sensitive: true` for **credential-shaped values** — API keys, tokens,
passwords, webhook secrets, connection strings with embedded credentials.

What it buys you:

- the platform UI **masks** the field;
- the generated `.example` carries a **placeholder**, never a real value.
  Non-sensitive keys may carry a real example value.

What it does **not** buy you: `sensitive` is a presentation and example-emission
flag. It is not encryption, not access control, and not a separate secret store.
The value still lands in a plain JSON file on the workspace volume and, if you
interpolate it, in the daemon's environment. If a value must not exist on the
box in plaintext, do not route it through plugin configuration at all.

> ### The flag is `sensitive`, not `secret` — and `secret` fails silently
>
> `configurationProperty` sets `additionalProperties: true`, so an unknown key is
> **accepted**. Writing `secret: true` does not error anywhere — it validates
> clean, and does nothing. The field is not masked and the `.example` emits the
> real value.
>
> Verified against the shipped schema: `sensitive` appears in it; the strings
> `secret`, `scope` and `immutable` appear **zero** times. Validating
> `{"api_key": {"type": "string", "secret": true}}` against the canonical
> `configurationContribution` branch returns **VALID**.

## What the vocabulary deliberately does *not* include

Free-form configuration dropped the wider vocabulary an earlier draft carried —
there is no `secret`, no `scope`, no `immutable`. The declared property keys are
exactly: `type`, `default`, `description`, `enum`, `sensitive`, `required`.

The value space itself is **free-form per plugin**: these are your keys, and
nothing in the platform constrains what a template may set. The property is an
open `anyOf` — a canonical well-formed branch plus a tolerant
always-satisfiable branch — mirroring `daemons` and `digestProviders`.

That openness is deliberate and load-bearing. The runtime drops bad keys and
keeps the plugin, so schema-constraining this property would *change* runtime
semantics rather than capture them, and would let a typo brick a plugin at the
(future, fail-closed) install gate. Declare the canonical shape anyway — the
emitters, the `.example` generator and the plugin tab all read it — but do not
expect the schema to catch your mistakes. It will not.

## Worked example — `molecule-scheduler`

The real declaration shipped in `molecule-ai-plugin-scheduler`'s `plugin.yaml`,
the first production adopter:

```yaml
name: molecule-scheduler
kind: trigger
contributes:
  configuration:
    title: Scheduler
    description: >-
      Tuning for the per-workspace scheduling daemon, and the schedules it
      SEEDS. The live grid remains volume-authoritative state owned by the
      workspace — `schedules` here is the seed that reconciles INTO it, never
      a replacement for it.
    properties:
      poll_seconds:
        type: integer
        default: 30
        description: >-
          How often the daemon scans the grid for due schedules, in seconds.
          The daemon clamps this to a 1s floor and falls back to 30 if the
          value is missing or unparseable, so a bad setting degrades rather
          than stopping the scheduler.
      schedules:
        type: array
        default: []
        description: >-
          Schedules this install seeds into the workspace's grid. Each entry is
          {name, cron (or the authoring alias cron_expr), prompt or prompt_file,
          timezone?, enabled?}.
  daemons:
    - name: scheduler
      command: python
      args: [scheduler.py]
      env:
        MOLECULE_TRIGGER_POLL_SECONDS: "${config:poll_seconds}"
```

Four things worth copying from it:

1. **Neither key is `sensitive`.** A poll interval and a schedule list are not
   credential-shaped, so the UI shows them and the `.example` can carry real
   values. Reach for `sensitive` when the value is a credential, not merely
   because it is operational.
2. **`poll_seconds` is consumed by interpolation, `schedules` is not.** An
   array does not flatten into an env var; the daemon reads it from the settings
   file. Mixing the two consumption styles in one plugin is normal.
3. **`schedules` defaults to an empty list on purpose.** Shipping entries would
   auto-fire them in every workspace that installs the plugin. This plugin ships
   the scheduling *daemon*, not preset schedules — real schedules belong in a
   workspace template.
4. **`default: 30` is declared even though the daemon already falls back to 30.**
   The declaration is what the plugin tab and the generated `.example` render
   from; leaving it implicit in code makes the key invisible to every consumer
   but the daemon itself.

## Related

- [`template-plugin-config.md`](template-plugin-config.md) — setting the values
  from a workspace template, and the two-identity trap
- [`sources.md`](sources.md) — plugin install sources
- `docs/runbooks/scheduler-plugin.md` — operating the scheduler plugin
