package handlers

// template_plugins_test.go — unit tests for parseTemplatePlugins +
// seedTemplatePlugins (RFC#2843 #32). Proves a workspace created directly via
// WorkspaceHandler.Create from a template that declares plugins:
//   1. parses the template config.yaml's `plugins:` block, and
//   2. WRITES the workspace_declared_plugins rows the post-online reconcile
//      needs (the gap this change closes: recordDeclaredPlugin previously ran
//      only in the org/import path, so a singly-provisioned seo-agent got no
//      declared rows → reconcile no-op → seo-all never installed).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestParseTemplatePlugins_AbsentFile(t *testing.T) {
	got, err := parseTemplatePlugins(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for absent config.yaml, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice, got %#v", got)
	}
}

func TestParseTemplatePlugins_EmptyPath(t *testing.T) {
	got, err := parseTemplatePlugins("")
	if err != nil {
		t.Fatalf("expected nil error for empty path, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for empty path, got %#v", got)
	}
}

func TestParseTemplatePlugins_NoPluginsBlock(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: Some Template
runtime: claude-code
model: foo/bar
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("expected nil error when plugins: absent, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero plugins, got %d: %v", len(got), got)
	}
}

// TestParseTemplatePlugins_SeoAgentShape pins the real seo-agent template
// config.yaml shape: a top-level `plugins:` list with the seo-all gitea source.
func TestParseTemplatePlugins_SeoAgentShape(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: SEO Agent
runtime: claude-code
plugins:
  - gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main
runtime_config:
  model: moonshot/kimi-k2.6
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("parseTemplatePlugins: %v", err)
	}
	want := "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("expected [%q], got %v", want, got)
	}
}

// TestParseTemplatePlugins_DedupAndOptOut pins the mergePlugins-aligned
// semantics: duplicate sources collapse and a leading "!"/"-" opts a plugin
// out (matching the org/import path).
func TestParseTemplatePlugins_DedupAndOptOut(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: T
plugins:
  - local://alpha
  - local://beta
  - local://alpha
  - "!local://beta"
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("parseTemplatePlugins: %v", err)
	}
	if len(got) != 1 || got[0] != "local://alpha" {
		t.Fatalf("expected dedup + beta opted out → [local://alpha], got %v", got)
	}
}

// TestSeedTemplatePlugins_WritesDeclaredRows is the load-bearing FIX-B proof:
// seedTemplatePlugins derives the install name from each source and UPSERTS a
// workspace_declared_plugins row. Uses sqlmock so the actual INSERT (and its
// derived plugin_name) is asserted — recordDeclaredPlugin no-ops when db.DB is
// nil, so a DB-backed test is required to prove the write really happens.
func TestSeedTemplatePlugins_WritesDeclaredRows(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	source := "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main"
	// gitea source with subpath → install name is the last subpath segment:
	// "seo-all". The upsert keys on (workspace_id, plugin_name) and stores the
	// full source string in source_raw.
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs("ws-create-1", "seo-all", source).
		WillReturnResult(sqlmock.NewResult(1, 1))

	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-create-1", []string{source})
	if recorded != 1 || skipped != 0 {
		t.Fatalf("expected recorded=1 skipped=0, got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (the declared row was not written as expected): %v", err)
	}
}

// TestSeedTemplatePlugins_SkipsUnparseableSource proves a bad source is skipped
// (logged) rather than aborting the rest of the declared set — and crucially
// does NOT issue a DB write for it.
func TestSeedTemplatePlugins_SkipsUnparseableSource(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// A scheme with no known naming rule → PluginNameFromSource errors → skip.
	// No ExpectExec is programmed; sqlmock fails the test if any unexpected
	// INSERT fires.
	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-create-1", []string{"bogus-scheme://whatever"})
	if recorded != 0 || skipped != 1 {
		t.Fatalf("expected recorded=0 skipped=1 for an unparseable source, got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestPayloadDeclaredPlugins_SeedsViaMergeAndSeed pins the chain the Create
// handler runs for CreateWorkspacePayload.Plugins: mergePlugins(nil, payload) →
// seedTemplatePlugins. This is the DURABLE declare channel that lands a plugin in
// workspace_declared_plugins (the SSOT provision recomputes MOLECULE_DECLARED_PLUGINS
// from) — unlike a MOLECULE_DECLARED_PLUGINS value injected via `secrets`, which
// provision overwrites. A duplicate entry must collapse to ONE upsert (mergePlugins
// dedup), proving the payload path is byte-identical to the template `plugins:` path.
func TestPayloadDeclaredPlugins_SeedsViaMergeAndSeed(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	source := "gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.1.0"
	// mergePlugins dedups → exactly ONE INSERT for the digest-mail install name.
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs("ws-create-1", "molecule-ai-plugin-digest-mail", source).
		WillReturnResult(sqlmock.NewResult(1, 1))

	declared := mergePlugins(nil, []string{source, source}) // caller-supplied dup
	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-create-1", declared)
	if recorded != 1 || skipped != 0 {
		t.Fatalf("expected recorded=1 skipped=0 (dedup to one row), got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (payload-declared digest row not written): %v", err)
	}
}

// nDistinctPluginSources builds n distinct gitea plugin sources (each derives a
// distinct install name), so mergePlugins keeps all n — a worst-case declared set.
func nDistinctPluginSources(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("gitea://molecule-ai/molecule-ai-plugin-p%d#v1", i)
	}
	return out
}

// TestPayloadPluginsToSeed_CapsAtMaxTemplatePlugins is the DoS bound: the payload
// `plugins:` path must enforce the SAME maxTemplatePlugins cap parseTemplatePlugins
// applies to the template path, and — matching that convention — REJECT an over-cap
// set entirely (ok=false, nil) rather than seed an unbounded number of rows. A set
// at exactly the cap passes through untouched.
//
// Negative control: against the pre-fix code the payload path was a bare
// mergePlugins(nil, payload.Plugins) with NO cap, so an over-cap set flowed
// straight into seedTemplatePlugins (len == maxTemplatePlugins+50 rows). This test
// asserts ok=false + nil for that same input, which the uncapped pre-fix path could
// not produce.
func TestPayloadPluginsToSeed_CapsAtMaxTemplatePlugins(t *testing.T) {
	over := nDistinctPluginSources(maxTemplatePlugins + 50)
	if got, ok := payloadPluginsToSeed(over); ok || got != nil {
		t.Fatalf("payload with %d distinct plugins (cap %d) must be REJECTED (ok=false, nil), got ok=%v len=%d — unbounded seed still possible",
			len(over), maxTemplatePlugins, ok, len(got))
	}

	atCap := nDistinctPluginSources(maxTemplatePlugins)
	got, ok := payloadPluginsToSeed(atCap)
	if !ok || len(got) != maxTemplatePlugins {
		t.Fatalf("payload at exactly the %d-plugin cap must pass unchanged, got ok=%v len=%d", maxTemplatePlugins, ok, len(got))
	}

	// Dedup still applies below the cap (byte-aligned with the template path).
	deduped, ok := payloadPluginsToSeed([]string{"local://a", "local://a", "local://b"})
	if !ok || len(deduped) != 2 {
		t.Fatalf("under-cap payload must dedup to 2, got ok=%v %v", ok, deduped)
	}
}

// TestPayloadDeclaredPlugins_OverCapRecordsZeroRows is the end-to-end bound proof:
// an over-cap payload, run through the SAME guard the Create handler uses
// (payloadPluginsToSeed → skip on !ok), writes ZERO workspace_declared_plugins
// rows. The mock DB programs NO ExpectExec, so any INSERT fails the test.
//
// Negative control: the pre-fix handler ran mergePlugins(nil, payload) →
// seedTemplatePlugins unconditionally, which would have attempted
// maxTemplatePlugins+50 INSERTs here and tripped sqlmock on the first unprogrammed
// write.
func TestPayloadDeclaredPlugins_OverCapRecordsZeroRows(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	// No ExpectExec programmed: an over-cap set must not reach seedTemplatePlugins.

	over := nDistinctPluginSources(maxTemplatePlugins + 50)
	declared, ok := payloadPluginsToSeed(over)
	if ok {
		// Mirror the handler guard: only seed when ok. If the cap regressed, this
		// branch would attempt INSERTs and sqlmock would fail below.
		seedTemplatePlugins(context.Background(), "ws-overcap", declared)
	}
	if ok {
		t.Fatalf("over-cap payload must be rejected before seeding (ok=false), got ok=true len=%d", len(declared))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB writes for an over-cap payload: %v", err)
	}
}

// TestPayloadDeclaredPlugins_PrivilegedRefused is the security negative-control:
// the payload plugins field must NOT become a privilege-escalation surface. The
// concierge management MCP (org-admin tools) declared via this path on a
// non-platform workspace must be REFUSED by recordDeclaredPlugin's kind-gate — no
// INSERT — exactly as the template/org-import paths are, since all share the same
// chokepoint.
func TestPayloadDeclaredPlugins_PrivilegedRefused(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// The kind precheck resolves the target workspace as an ordinary workspace.
	mock.ExpectQuery(`SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-ordinary").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
	// No ExpectExec is programmed: an INSERT here would be a privilege escalation
	// and sqlmock fails the test on any unexpected write.

	declared := mergePlugins(nil, []string{conciergePlatformMCPSource})
	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-ordinary", declared)
	if recorded != 0 || skipped != 1 {
		t.Fatalf("privileged plugin on a non-platform workspace must be refused (recorded=0 skipped=1), got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (kind-gate should have blocked the write): %v", err)
	}
}
