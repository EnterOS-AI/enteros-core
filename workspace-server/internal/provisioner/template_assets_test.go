package provisioner

// template_assets_test.go — generic template-asset channel tests
// (RFC #2843 #24). Verifies:
//   1. The fetcher wires into collectCPConfigFiles when both
//      cfg.TemplateAssetFetcher and cfg.TemplateIdentity are set.
//   2. Fetched assets MERGE with cfg.ConfigFiles (caller wins
//      on conflict — same pattern as the persisted-bundle
//      provider in #2831 PIECE 1).
//   3. A transport error on the fetcher ABORTS the provision
//      (fail-closed; never regresses to stub /configs).
//   4. Nil fetcher = no-op (self-host default; the existing
//      TemplatePath local-dir path still works).
//   5. Empty TemplateIdentity with a non-nil fetcher = no-op
//      (the fetcher is only called when there's an identity
//      to resolve).

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// fakeTemplateAssetFetcher is a capture-only stub satisfying
// TemplateAssetFetcher. Returns the configured bundle+err;
// records the template identities the handler asked for.
type fakeTemplateAssetFetcher struct {
	bundle map[string][]byte
	err    error
	calls  []string
}

func (f *fakeTemplateAssetFetcher) Load(_ context.Context, templateIdentity string) (map[string][]byte, error) {
	f.calls = append(f.calls, templateIdentity)
	return f.bundle, f.err
}

// TestCollectCPConfigFiles_MergesFetcherAssets is the
// happy path: fetcher returns assets, the bundle includes
// them alongside any caller-supplied cfg.ConfigFiles.
// Fails if a future refactor stops calling cfg.TemplateAssetFetcher.Load
// from collectCPConfigFiles when the fetcher + identity are set.
func TestCollectCPConfigFiles_MergesFetcherAssets(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml":                  []byte("# from template repo"),
			"prompts/system.md":            []byte("# template system prompt"),
			"agent-skills/seo-audit/SKILL.md": []byte("# seo skill"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	files, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	// All 3 fetched assets land in the bundle.
	wantKeys := []string{
		"config.yaml",
		"prompts/system.md",
		"agent-skills/seo-audit/SKILL.md",
	}
	for _, wk := range wantKeys {
		if _, ok := files[wk]; !ok {
			t.Errorf("expected %q in bundle, got keys: %v", wk, keysOfBundle(files))
		}
	}
	// Fetcher was called once with the right identity.
	if len(prov.calls) != 1 || prov.calls[0] != "seo-agent-v1.2.3" {
		t.Errorf("expected one Load call with identity=seo-agent-v1.2.3, got calls=%v", prov.calls)
	}
}

// TestCollectCPConfigFiles_CallerWinsOnConflict asserts the
// no-clobber policy: when the caller supplies a
// cfg.ConfigFiles["config.yaml"] AND the fetcher returns the
// same key, the caller's value wins (matches the persisted-
// bundle provider pattern in #2831 PIECE 1).
func TestCollectCPConfigFiles_CallerWinsOnConflict(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml": []byte("# from template repo"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
		ConfigFiles: map[string][]byte{
			"config.yaml": []byte("# caller override"),
		},
	}

	files, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	// Decode the base64-encoded bundle value.
	decoded, decErr := base64DecodeFirst(files["config.yaml"])
	if decErr != nil {
		t.Fatalf("decode config.yaml: %v", decErr)
	}
	if string(decoded) != "# caller override" {
		t.Errorf("expected caller override to win, got bundle config.yaml=%q", string(decoded))
	}
}

// TestCollectCPConfigFiles_FetcherErrorAborts is the
// fail-closed assertion: a transport error from the fetcher
// must abort the provision rather than regressing to stub
// /configs (the same fail-closed contract as the
// persisted-bundle provider in #2831 PIECE 1). If a future
// refactor swallows the fetch error, the bundle would
// silently miss the agent-skills/ files and the workspace
// would boot with a stub config — the exact regression
// this test guards against.
func TestCollectCPConfigFiles_FetcherErrorAborts(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{err: errors.New("gitea 503")}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	_, err := collectCPConfigFiles(cfg)
	if err == nil {
		t.Fatal("expected collectCPConfigFiles to abort on fetcher error, got nil")
	}
	if !stringsContains(err.Error(), "gitea 503") {
		t.Errorf("expected error to surface the underlying gitea 503, got: %v", err)
	}
}

// TestCollectCPConfigFiles_NilFetcherIsNoop asserts the
// self-host default: a WorkspaceConfig without a fetcher
// does NOT call anything (the existing TemplatePath +
// ConfigFiles path is unchanged). The RFC's opt-in
// contract: nil fetcher = no asset channel, no behavior
// change for self-host callers.
func TestCollectCPConfigFiles_NilFetcherIsNoop(t *testing.T) {
	cfg := WorkspaceConfig{
		TemplateIdentity: "seo-agent-v1.2.3", // set but no fetcher wired
	}
	_, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles with nil fetcher: %v", err)
	}
}

// TestCollectCPConfigFiles_EmptyIdentityNoop asserts that
// even a wired fetcher is NOT called when TemplateIdentity is
// empty (the Gitea fetcher needs an identity to resolve; an
// empty identity would be a programming error and should
// be a no-op rather than a fetch-with-empty-identity call).
func TestCollectCPConfigFiles_EmptyIdentityNoop(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{"config.yaml": []byte("# unexpected")},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "", // empty
		TemplateAssetFetcher: prov, // wired but no identity
	}
	_, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles with empty identity: %v", err)
	}
	if len(prov.calls) != 0 {
		t.Errorf("expected fetcher NOT to be called with empty identity, got calls=%v", prov.calls)
	}
}

// base64DecodeFirst is a small helper that decodes the
// first base64-encoded value in the bundle, returning the
// raw bytes. Test-side helper to avoid pulling base64
// decoding into the production path.
func base64DecodeFirst(v string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

// keysOfBundle returns the sorted keys of a map for stable
// test output. Local helper to avoid coupling to the test
// files' key-ordering conventions.
func keysOfBundle(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// stringsContains is a tiny shim so the test doesn't need
// the strings import elsewhere (it does already, but this
// keeps the dependency local). Existence is asserted via
// string comparison.
func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}
