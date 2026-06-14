package provisioner

// template_assets.go — generic template-asset channel (RFC #2843 #24).
//
// This is the "generic, non-secret asset channel" the RFC proposes:
// a workspace's template assets (config.yaml + prompts/ + agent-skills/)
// are materialized from a template identity (resolved by the
// caller — the template repo path, a cached ref, etc.) rather
// than forced through the AWS Secrets Manager config bundle path
// that caps at cpConfigFilesMaxBytes (256 KiB) and silently drops
// any skill over the cap.
//
// The fetcher is interface-typed so tests inject fakes and the
// real implementation (in main.go) wires the Gitea shallow-clone
// per RFC §4.2 transport option (a). The interface is NARROW
// (Load only) to keep the abstraction minimal — the template
// identity resolution and fetch are both the caller's job.
//
// Concurrency: the fetcher's Load is called from
// prepareProvisionContext, which serializes per-workspace
// (the existing per-workspace restart/provision gate at
// workspace_dispatchers.go:439 holds the mutex). No
// additional locking needed.

import "context"

// TemplateAssetFetcher materializes a template's
// config.yaml + prompts/ + agent-skills/ from a non-secret
// asset channel (template repo, Gitea shallow clone per
// RFC #2843 §4.2). Returned paths are RELATIVE to the
// template asset root (e.g. "config.yaml", "prompts/system.md",
// "agent-skills/seo-audit/SKILL.md") and the bytes are
// raw file contents (not base64-encoded — the caller can
// re-encode if needed for the SM wire format; the generic
// channel does not require encoding).
//
// Returned errors: a transport / resolution failure is
// returned as a non-nil error so the caller can abort the
// provision rather than silently regressing to stub-mode
// /configs (the same fail-closed contract as the persisted-
// bundle provider in #2831 PIECE 1).
type TemplateAssetFetcher interface {
	Load(ctx context.Context, templateIdentity string) (map[string][]byte, error)
}

// defaultTemplateAssetFetcher is the in-tree fallback used
// when no provider is wired (self-host default — the
// Docker provisioner reads from a local TemplatePath
// directory; SaaS wires a Gitea fetcher in main.go).
// Currently this is a stub that returns nil — the existing
// TemplatePath path in collectCPConfigFiles continues to
// handle the local-dir case. Wire a real impl in #24's
// follow-ups (the SaaS-side Gitea fetch).
type defaultTemplateAssetFetcher struct{}

// Load on defaultTemplateAssetFetcher returns (nil, nil) —
// "no assets to add." The existing collectCPConfigFiles
// TemplatePath walk handles the local-dir case for self-host.
// SaaS workspaces wire a real fetcher (Gitea shallow clone)
// in main.go per RFC #2843 §4.2 transport option (a).
func (defaultTemplateAssetFetcher) Load(_ context.Context, _ string) (map[string][]byte, error) {
	return nil, nil
}
