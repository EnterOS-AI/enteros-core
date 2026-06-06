package handlers

// llm_billing_mode_derived_test.go — tests for the DERIVED billing-mode
// resolver (internal#718 P2-B). The platform-vs-byok decision now DERIVES the
// provider from (runtime, model) via the provider registry and keys off
// IsPlatform(derived) — it does NOT read a stored LLM_PROVIDER (supersedes
// #1966's stored-read approach) and does NOT read the org rung (retired,
// CTO 2026-05-27). `workspaces.llm_billing_mode` survives ONLY as an optional
// explicit operator override (first precedence).
//
// This file pins the explicit BEHAVIOR DELTA the RFC's P2 calls out:
//   - platform-derived (or unset → platform default) → platform_managed (UNCHANGED)
//   - non-platform-derived                            → byok (THE FIX — the Reno leak class)
//   - explicit override                               → wins over derive
//   - derive error / unregistered                     → platform_managed (default-closed)

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectOverrideQuery sets up the workspaces.llm_billing_mode override read
// (first precedence). value=="" means NULL (no override).
func expectOverrideQuery(m sqlmock.Sqlmock, wsID, value string) {
	rows := sqlmock.NewRows([]string{"llm_billing_mode"})
	if value == "" {
		rows.AddRow(nil)
	} else {
		rows.AddRow(value)
	}
	m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(rows)
}

func TestResolveLLMBillingModeDerived_BehaviorDelta(t *testing.T) {
	ctx := context.Background()
	const wsID = "33333333-3333-3333-3333-333333333333"

	type tc struct {
		name       string
		runtime    string
		model      string
		authEnv    []string
		override   string // "" = NULL override (no explicit operator override)
		wantMode   string
		wantSource BillingModeSource
		wantErr    bool
	}

	cases := []tc{
		{
			// PLATFORM-DERIVED → platform_managed (UNCHANGED). claude-code +
			// a platform-namespaced model id derives to the closed `platform`
			// provider → IsPlatform → platform_managed.
			name:       "platform_derived_keeps_platform_managed_UNCHANGED",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// NON-PLATFORM-DERIVED → byok (THE FIX). claude-code + the
			// kimi-coding-native model derives to the non-platform kimi-coding
			// provider → IsPlatform=false → byok. This is the Reno billing-leak
			// class: pre-P2 it resolved platform_managed and ran on platform creds.
			name:       "non_platform_derived_resolves_byok_THE_FIX",
			runtime:    "claude-code",
			model:      "kimi-for-coding",
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// NON-PLATFORM vendor on codex: gpt-5.5 derives to `openai` (BYOK).
			name:       "non_platform_openai_codex_byok",
			runtime:    "codex",
			model:      "gpt-5.5",
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// PLATFORM-DERIVED on codex: openai/gpt-5.4 is platform-namespaced.
			name:       "platform_derived_codex_platform_managed",
			runtime:    "codex",
			model:      "openai/gpt-5.4",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// UNSET model → platform default (CTO-confirmed "unset → platform
			// default"). No model means nothing to derive; default-closed.
			name:       "unset_model_platform_default",
			runtime:    "claude-code",
			model:      "",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// UNREGISTERED model → derive errors → platform default (default-closed,
			// NOT a silent byok flip that would strip a workspace's creds).
			name:       "unregistered_model_derive_error_platform_default",
			runtime:    "claude-code",
			model:      "totally-made-up-model-xyz",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// UNKNOWN runtime → derive errors → platform default (default-closed).
			name:       "unknown_runtime_platform_default",
			runtime:    "no-such-runtime",
			model:      "claude-opus-4-7",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// EXPLICIT OVERRIDE wins over derive: a non-platform-deriving model
			// kept on platform_managed by an operator override (escape hatch).
			name:       "explicit_override_platform_managed_wins_over_byok_derive",
			runtime:    "claude-code",
			model:      "kimi-for-coding", // would derive byok
			override:   LLMBillingModePlatformManaged,
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// EXPLICIT OVERRIDE byok wins over a platform-deriving model.
			name:       "explicit_override_byok_wins_over_platform_derive",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7", // would derive platform_managed
			override:   LLMBillingModeBYOK,
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// EXPLICIT OVERRIDE disabled wins (no-LLM workspace).
			name:       "explicit_override_disabled_wins",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7",
			override:   LLMBillingModeDisabled,
			wantMode:   LLMBillingModeDisabled,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// AUTH-ENV disambiguation: claude-code's anthropic-oauth (alias
			// model "opus") vs anthropic-api both could match a bare alias; with
			// CLAUDE_CODE_OAUTH_TOKEN present it derives anthropic-oauth → byok.
			name:       "auth_env_disambiguates_oauth_byok",
			runtime:    "claude-code",
			model:      "opus",
			authEnv:    []string{"CLAUDE_CODE_OAUTH_TOKEN"},
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock := setupTestDB(t)
			expectOverrideQuery(mock, wsID, c.override)

			res, err := ResolveLLMBillingModeDerived(ctx, wsID, c.runtime, c.model, c.authEnv)
			if (err != nil) != c.wantErr {
				t.Fatalf("err: got %v wantErr=%v", err, c.wantErr)
			}
			if res.ResolvedMode != c.wantMode {
				t.Errorf("mode: got %q want %q", res.ResolvedMode, c.wantMode)
			}
			if res.Source != c.wantSource {
				t.Errorf("source: got %q want %q", res.Source, c.wantSource)
			}
			if !isKnownBillingMode(res.ResolvedMode) {
				t.Errorf("post-condition: resolved mode %q not a known enum", res.ResolvedMode)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestResolveLLMBillingModeDerived_OverrideDBError_DefaultClosed asserts a DB
// error reading the override column defaults closed to platform_managed and
// propagates the error — never silently flips a workspace off platform creds.
func TestResolveLLMBillingModeDerived_OverrideDBError_DefaultClosed(t *testing.T) {
	ctx := context.Background()
	const wsID = "44444444-4444-4444-4444-444444444444"

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnError(errors.New("connection refused"))

	res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "kimi-for-coding", nil)
	if err == nil {
		t.Fatalf("expected propagated DB error, got nil")
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("default-closed: DB error must resolve platform_managed, got %q", res.ResolvedMode)
	}
	if res.Source != BillingModeSourceConstantFallback {
		t.Errorf("source: got %q want %q", res.Source, BillingModeSourceConstantFallback)
	}
}

// TestResolveLLMBillingModeDerived_EmptyWorkspaceID_PlatformDefault asserts the
// pre-provision context (no workspace id, no override read) defaults to
// platform_managed without a DB query.
func TestResolveLLMBillingModeDerived_EmptyWorkspaceID_PlatformDefault(t *testing.T) {
	ctx := context.Background()
	mock := setupTestDB(t) // no query expected
	res, err := ResolveLLMBillingModeDerived(ctx, "", "claude-code", "kimi-for-coding", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("empty workspace id must default platform_managed, got %q", res.ResolvedMode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
