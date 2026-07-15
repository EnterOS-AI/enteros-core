# Plugin install sources

The platform currently registers three plugin source schemes: `local`,
`github`, and `gitea`. Source resolution only fetches and stages files; the
workspace runtime then interprets the plugin shape through its adapter registry.

## API

`POST /workspaces/:id/plugins` accepts a `source` value. Examples:

```json
{"source":"local://my-plugin"}
{"source":"github://owner/repo#sha:0123456789abcdef"}
{"source":"gitea://molecule-ai/repo/plugins/my-plugin#sha:0123456789abcdef"}
```

`GET /plugins/sources` reports the schemes wired into the running server. The
default response contains `gitea`, `github`, and `local`; deployments may add
more resolvers.

## Built-in resolvers

### `local`

`local://<name>` and a bare `<name>` read a plugin from the configured local
plugins directory. Names are path-safe and symlinks are not followed while
copying.

### `github`

```text
github://<owner>/<repo>#<ref>
```

The GitHub resolver clones a repository with the system `git` client. It is
anonymous by default. A ref is required in normal deployments; unpinned sources
are allowed only when `PLUGIN_ALLOW_UNPINNED=true` is explicitly set for local
development.

### `gitea`

```text
gitea://<owner>/<repo>[/<subpath>]#<ref>
```

The Gitea resolver supports whole repositories or a validated subdirectory. It
uses `MOLECULE_TEMPLATE_REPO_TOKEN` at fetch time for repositories that require
authentication. The token is sent in an authorization header or redacted URL
path and must not appear in logs or error responses.

The Gitea resolver also requires a ref in normal deployments. Use an immutable
form such as `#sha:<full-commit>` or `#tag:<version>` when reproducibility is
required. A branch ref is accepted and is tracked by resolved commit SHA; it is
not immutable and may be re-delivered when the branch tip moves.

## Supply-chain boundaries

The install pipeline currently enforces:

- validated schemes, owner/repository names, subpaths, and refs;
- pinned remote refs unless the explicit development override is enabled;
- bounded request, fetch duration, and staged-directory size;
- credential redaction from resolver errors and git command output;
- optional caller-supplied content-integrity verification;
- a validated plugin name before any workspace path is constructed.

Installing a plugin remains a code-execution grant to the target workspace.
Review the source and pin it like any other dependency. The platform does not
provide a general resolver network sandbox or signatures for arbitrary plugin
trees.

## Adding a resolver

A resolver implements:

```go
type SourceResolver interface {
    Scheme() string
    Fetch(ctx context.Context, spec, dst string) (string, error)
}
```

Register it with `PluginsHandler.WithSourceResolver` before serving requests.
It must honor context cancellation, validate input before network access,
remove temporary state, and avoid leaking credentials.

See [Plugin shapes and Agent Skills compatibility](./agentskills-compat.md) for
what happens after the source tree reaches a workspace.
