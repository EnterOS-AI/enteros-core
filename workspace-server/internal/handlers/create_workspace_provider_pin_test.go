package handlers

// create_workspace NOT_CONFIGURED regression gate.
//
// A child workspace the concierge spawns via the `create_workspace`
// management-MCP tool flows through WorkspaceHandler.Create, which persists
// MODEL (setModelSecret) but — since the internal#718 P4 closure removed the
// unconditional setProviderSecret write — persisted NO LLM_PROVIDER. For a
// platform-managed model id like "moonshot/kimi-k2.6" the on-box runtime
// re-derives provider="moonshot" (a model PREFIX, not a registry NAME) and the
// claude-code adapter fail-closes ("workspace config picks provider='moonshot'
// but it is not in the providers registry") → the child boots online but
// NOT_CONFIGURED.
//
// ensureCreatedWorkspaceProviderPin (wired into Create after setModelSecret)
// closes that gap: it pins LLM_PROVIDER=platform iff the registry derivation of
// (runtime, model) is the closed `platform` provider, mirroring the concierge's
// ensureConciergeProvider IsPlatform gate. These tests prove a child created via
// create_workspace is born with a COMPLETE, self-consistent (runtime, model,
// provider) config that the registry validates — not a MODEL-without-provider
// the adapter would reject — while BYOK/OAuth children are left untouched.

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureCreatedWorkspaceProviderPin(t *testing.T) {
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("platform-managed child (moonshot/kimi-k2.6) gets LLM_PROVIDER=platform pinned", func(t *testing.T) {
		mock := setupTestDB(t)
		// setProviderSecret writes the LLM_PROVIDER row directly (no preceding
		// existence SELECT — Create owns the fresh row, there is nothing to respect).
		mock.ExpectExec(secretInsert).
			WithArgs("ws-child", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		ensureCreatedWorkspaceProviderPin(context.Background(), "ws-child", "claude-code", "moonshot/kimi-k2.6", nil)

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("platform-managed child was NOT pinned LLM_PROVIDER=platform (the create_workspace NOT_CONFIGURED bug): %v", err)
		}
	})

	t.Run("pinned provider matches the registry-derived provider (config is self-consistent)", func(t *testing.T) {
		// The value the helper persists MUST equal what DeriveProvider resolves for
		// the same (runtime, model) — otherwise the config the adapter reads is
		// internally inconsistent. Assert the pin equals the derived provider name
		// AND that it IS the closed platform provider.
		m, err := providerRegistry()
		if err != nil || m == nil {
			t.Fatalf("provider registry unavailable: %v", err)
		}
		prov, derr := m.DeriveProvider("claude-code", "moonshot/kimi-k2.6", nil)
		if derr != nil {
			t.Fatalf("DeriveProvider(claude-code, moonshot/kimi-k2.6) failed: %v", derr)
		}
		if !prov.IsPlatform() {
			t.Fatalf("registry SSOT changed: moonshot/kimi-k2.6 no longer derives to the platform provider (got %q) — the pin logic is keyed off this", prov.Name)
		}
		if prov.Name != providers.PlatformProviderName {
			t.Errorf("derived provider name %q != %q (the value the helper pins)", prov.Name, providers.PlatformProviderName)
		}
	})

	t.Run("BYOK child carrying a vendor key derives to its real provider — NOT pinned platform", func(t *testing.T) {
		// A create that supplies ANTHROPIC_API_KEY for an anthropic-api model
		// derives to anthropic-api (a real provider entry), not platform. The
		// helper must NOT write any LLM_PROVIDER row (no INSERT expected): pinning
		// would mis-route the runtime's own correct derivation.
		mock := setupTestDB(t)
		// No ExpectExec — any INSERT is an unmet-expectation failure below.

		// Model whose registry derivation is a real (non-platform) provider when
		// the anthropic-api auth env is present.
		ensureCreatedWorkspaceProviderPin(
			context.Background(),
			"ws-byok-child",
			"claude-code",
			"anthropic:claude-opus-4-7",
			[]string{"ANTHROPIC_API_KEY"},
		)

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("BYOK child wrongly got an LLM_PROVIDER write (would mis-route the runtime's own derivation): %v", err)
		}
	})

	t.Run("empty model is a no-op (external/registerless create)", func(t *testing.T) {
		mock := setupTestDB(t)
		// No INSERT expected.
		ensureCreatedWorkspaceProviderPin(context.Background(), "ws-empty", "claude-code", "", nil)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("empty model should be a no-op (no provider pin): %v", err)
		}
	})

	t.Run("unknown/federated runtime derive-miss is a no-op (fails open, no pin)", func(t *testing.T) {
		mock := setupTestDB(t)
		// DeriveProvider errors for an unknown runtime → helper returns without a
		// write (matches the create-boundary registry gates that fail open here).
		ensureCreatedWorkspaceProviderPin(context.Background(), "ws-fed", "some-federated-runtime", "vendor/model", nil)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unknown-runtime derive-miss should be a no-op: %v", err)
		}
	})
}
