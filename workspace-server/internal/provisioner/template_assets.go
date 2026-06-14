package provisioner

// template_assets.go — generic template-asset channel (RFC #2843 #24).
//
// This is the "generic, non-secret asset channel" the RFC proposes:
// a workspace's template assets (config.yaml + prompts/ + agent-skills/)
// are materialized from a template identity (resolved by the caller —
// the template repo path, a cached ref, etc.) rather than forced
// through the AWS Secrets Manager config bundle path that caps at
// cpConfigFilesMaxBytes (256 KiB) and silently drops any skill over
// the cap.
//
// The fetcher is interface-typed so tests inject fakes and the real
// implementation (in main.go) wires the Gitea shallow-clone per
// RFC §4.2 transport option (a). The interface is NARROW (Load only)
// to keep the abstraction minimal — the template identity resolution
// and fetch are both the caller's job.
//
// Blast-radius / isolation (Reviewer-CR2 RC #11690 on the prior
// head): the fetcher ONLY materializes TEMPLATE ASSETS. Every
// path the fetcher returns is gated by IsCPTemplateAssetPath
// (this file) BEFORE it lands in the wire payload. Paths outside
// the allowlist — MEMORY.md, USER.md, CLAUDE.md, .claude/sessions/,
// /etc/passwd, traversal sequences — are REJECTED (the provision
// aborts with a structured error) rather than silently admitted.
// This is the load-bearing guard: a fetcher that returns a path
// outside the template-asset namespace is a programming error or
// an attack, and either way the safe response is to fail closed.
//
// Transport split (Reviewer-CR2 addendum on the prior head):
// fetched assets go to a SEPARATE wire field (TemplateAssets on
// cpProvisionRequest) rather than being merged into ConfigFiles.
// ConfigFiles is the bundle the CP stages through AWS Secrets
// Manager — the wrong layer for non-secret assets per the
// core-devops 10:13 SM-inventory RCA. The split lets a future CP
// route TemplateAssets through a non-secret channel (Gitea asset
// pin, S3 non-secret bucket, etc.) without a wire-shape change.
//
// Concurrency: the fetcher's Load is called from prepareProvisionContext,
// which serializes per-workspace (the existing per-workspace
// restart/provision gate at workspace_dispatchers.go:439 holds the
// mutex). No additional locking needed.

import (
	"context"
	"path/filepath"
	"strings"
)

// TemplateAssetFetcher materializes a template's
// config.yaml + prompts/ + agent-skills/ from a non-secret
// asset channel (template repo, Gitea shallow clone per
// RFC #2843 §4.2). Returned paths are RELATIVE to the
// template asset root (e.g. "config.yaml", "prompts/system.md",
// "agent-skills/seo-audit/SKILL.md") and the bytes are raw
// file contents (not base64-encoded — the generic channel
// does not require encoding; the wire format encodes per
// its own transport).
//
// Returned errors: a transport / resolution failure is
// returned as a non-nil error so the caller can abort the
// provision rather than silently regressing to stub-mode
// /configs (the same fail-closed contract as the persisted-
// bundle provider in #2831 PIECE 1).
//
// CONTRACT: every key in the returned map MUST match
// IsCPTemplateAssetPath. Keys that don't match are rejected
// by the caller (the provision aborts). Implementations that
// can't constrain their output to the allowlist must filter
// before returning.
type TemplateAssetFetcher interface {
	Load(ctx context.Context, templateIdentity string) (map[string][]byte, error)
}

// IsCPTemplateAssetPath reports whether a path returned by a
// TemplateAssetFetcher is eligible for transport to the workspace.
//
// Allowlist (load-bearing blast-radius guard — see RC #11690):
//
//   - "config.yaml" — the runtime entrypoint config
//   - "prompts/*"   — system prompts
//   - "agent-skills/*" — the agent's skill packages
//
// Everything else is REJECTED. Specifically excluded:
// MEMORY.md / USER.md (curated durable memory — agent-owned
// state, reconciled by the boot entrypoint, not by this
// collect path), CLAUDE.md (runtime memory file, agent-owned),
// .claude/sessions/* (Claude Code session dir, agent-owned),
// anything outside the template-asset namespace.
//
// Path normalization: the function applies filepath.ToSlash
// + filepath.Clean before matching, so Windows-style
// separators, redundant "./" segments, and trailing slashes
// are normalized. Traversal sequences (".." or paths
// containing "/../") are NOT explicitly stripped here —
// callers must check for traversal before calling this
// function (the existing addFile in cp_provisioner.go does).
// This function only validates the namespace; the traversal
// check is a separate invariant.
func IsCPTemplateAssetPath(name string) bool {
	name = filepath.ToSlash(filepath.Clean(name))
	return name == "config.yaml" ||
		strings.HasPrefix(name, "prompts/") ||
		strings.HasPrefix(name, "agent-skills/")
}
