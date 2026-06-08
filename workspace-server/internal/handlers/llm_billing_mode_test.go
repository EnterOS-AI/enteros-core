package handlers

// llm_billing_mode_test.go — tests for the LEGACY-signature resolver
// ResolveLLMBillingMode after internal#718 P2-B. The org rung is RETIRED: the
// legacy shim now reads the explicit override first, then DERIVES the provider
// from the workspace's stored (runtime, model) via the registry (no org
// default). The dedicated derived-resolver cases live in
// llm_billing_mode_derived_test.go; this file pins the legacy shim's DB-read
// sequence + that it routes through the derived semantics.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectLegacyShimQueries sets up the DB reads the legacy ResolveLLMBillingMode
// shim makes on a NO-explicit-override path (internal#718 P2-B), in order:
//  1. override read (NULL) — the shim's own precedence-1 check,
//  2. workspaces.runtime read,
//  3. workspace_secrets scan (MODEL + auth-env names),
//  4. override read AGAIN (NULL) — the derived resolver re-checks it so it is a
//     complete, independently-callable SSOT.
//
// model=="" means no MODEL secret row.
func expectLegacyShimQueries(m sqlmock.Sqlmock, wsID, runtime, model string) {
	nullOverride := func() {
		m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))
	}
	nullOverride()
	m.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
	secretRows := sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"})
	if model != "" {
		secretRows.AddRow("MODEL", []byte(model), 0) // version 0 = plaintext
	}
	m.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(secretRows)
	nullOverride()
}

func TestResolveLLMBillingMode_LegacyShimDerives(t *testing.T) {
	withProxyConfigured(t) // SaaS context: default-closed → platform_managed.
	ctx := context.Background()
	const wsID = "11111111-1111-1111-1111-111111111111"

	type want struct {
		mode        string
		source      BillingModeSource
		hasOverride bool
	}
	type tc struct {
		name      string
		setupMock func(m sqlmock.Sqlmock)
		want      want
		wantErr   bool
	}

	cases := []tc{
		{
			// Explicit override still wins (first precedence; only stored signal
			// that survives P2-B). No runtime/secrets read needed.
			name: "explicit_override_byok_wins",
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceWorkspaceOverride, hasOverride: true},
		},
		{
			// No override + a non-platform-deriving model → byok via derive (THE
			// FIX: pre-P2 this was platform_managed via the org rung).
			name: "no_override_derives_byok_from_model",
			setupMock: func(m sqlmock.Sqlmock) {
				expectLegacyShimQueries(m, wsID, "claude-code", "kimi-for-coding")
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceDerivedProvider, hasOverride: false},
		},
		{
			// No override + a platform-namespaced model → platform_managed (UNCHANGED).
			name: "no_override_derives_platform_from_model",
			setupMock: func(m sqlmock.Sqlmock) {
				expectLegacyShimQueries(m, wsID, "claude-code", "anthropic/claude-opus-4-7")
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedProvider, hasOverride: false},
		},
		{
			// No override + no model → derived_default → platform_managed (unset → platform).
			name: "no_override_no_model_platform_default",
			setupMock: func(m sqlmock.Sqlmock) {
				expectLegacyShimQueries(m, wsID, "claude-code", "")
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedDefault, hasOverride: false},
		},
		{
			// Garbled override is NOT honored — falls through to derive
			// (default-closed). Here no model → platform default.
			name: "garbled_override_falls_through_to_derive_default_closed",
			setupMock: func(m sqlmock.Sqlmock) {
				// override read 1 (garbled → not honored), runtime, secrets,
				// override read 2 (garbled again, derived resolver re-check).
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("byokk"))
				m.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
				m.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("byokk"))
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedDefault, hasOverride: false},
		},
		{
			// DB error on the override read → default-closed + propagated error.
			name: "override_db_error_default_closed_with_error",
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnError(errors.New("connection refused"))
			},
			want:    want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceConstantFallback, hasOverride: false},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock := setupTestDB(t)
			c.setupMock(mock)

			// orgMode arg is retired/ignored; pass a value to prove it has no effect.
			res, err := ResolveLLMBillingMode(ctx, wsID, LLMBillingModeBYOK)
			if (err != nil) != c.wantErr {
				t.Fatalf("err: got %v wantErr=%v", err, c.wantErr)
			}
			if res.ResolvedMode != c.want.mode {
				t.Errorf("mode: got %q want %q", res.ResolvedMode, c.want.mode)
			}
			if res.Source != c.want.source {
				t.Errorf("source: got %q want %q", res.Source, c.want.source)
			}
			if (res.WorkspaceOverride != nil) != c.want.hasOverride {
				t.Errorf("hasOverride: got %v want %v", res.WorkspaceOverride != nil, c.want.hasOverride)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestResolveLLMBillingMode_EmptyWorkspaceID_PlatformDefault: pre-provision
// (no workspace id) defaults closed with no DB read (org rung retired, so the
// old "org_only" behavior is gone — it's now the platform default).
func TestResolveLLMBillingMode_EmptyWorkspaceID_PlatformDefault(t *testing.T) {
	withProxyConfigured(t) // SaaS context.
	ctx := context.Background()
	mock := setupTestDB(t) // no DB read expected
	res, err := ResolveLLMBillingMode(ctx, "", LLMBillingModeBYOK)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("empty ws id must default platform_managed, got %q", res.ResolvedMode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid asserts the resolver's
// post-condition: the returned mode is ALWAYS one of the three known enum
// values. The strip gate downstream relies on this so it can switch on
// res.ResolvedMode without a separate is-valid check on every call site.
func TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid(t *testing.T) {
	withProxyConfigured(t) // SaaS context: default-closed → platform_managed.
	ctx := context.Background()
	const wsID = "22222222-2222-2222-2222-222222222222"

	// Garbled override + no derivable model: must still resolve a known enum
	// (platform_managed, default-closed). Query order: override(garbled),
	// runtime, secrets, override(garbled again — derived resolver re-check).
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("totally-bogus"))
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("totally-bogus"))

	res, err := ResolveLLMBillingMode(ctx, wsID, "also-bogus")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !isKnownBillingMode(res.ResolvedMode) {
		t.Errorf("post-condition violated: resolved mode %q is not a known enum value", res.ResolvedMode)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("default-closed contract: garbled-override + no-model must resolve platform_managed, got %q", res.ResolvedMode)
	}
}

// TestSetWorkspaceLLMBillingMode_Validation guards the SET path. The CHECK
// constraint at the DB layer is the second line of defense; the route
// handler relies on this function rejecting unknown modes with a clean
// error (so it can map to 400) instead of letting them hit Postgres and
// surfacing as a sql-driver error string.
func TestSetWorkspaceLLMBillingMode_Validation(t *testing.T) {
	ctx := context.Background()
	const wsID = "33333333-3333-3333-3333-333333333333"

	t.Run("rejects_unknown_mode_without_db_call", func(t *testing.T) {
		setupTestDB(t) // mock expects nothing — the function must short-circuit
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, "totally-bogus"); err == nil {
			t.Fatal("expected error for unknown mode, got nil")
		}
	})

	t.Run("rejects_empty_workspace_id", func(t *testing.T) {
		setupTestDB(t)
		if err := SetWorkspaceLLMBillingMode(ctx, "", LLMBillingModeBYOK); err == nil {
			t.Fatal("expected error for empty workspace id, got nil")
		}
	})

	t.Run("clear_uses_NULL_update", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = \$1`).
			WithArgs(wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, ""); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("set_byok_uses_value_update", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
			WithArgs(LLMBillingModeBYOK, wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, LLMBillingModeBYOK); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
