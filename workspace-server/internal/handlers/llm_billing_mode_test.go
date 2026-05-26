package handlers

// llm_billing_mode_test.go — table-driven tests for the per-workspace
// resolver (internal#691). The cases below enumerate every documented
// branch in the default-closed contract; if one of them flips behavior
// later the test names will tell the reviewer exactly which RFC clause
// regressed.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestResolveLLMBillingMode_TableDriven(t *testing.T) {
	ctx := context.Background()
	const wsID = "11111111-1111-1111-1111-111111111111"

	type want struct {
		mode   string
		source BillingModeSource
		// hasOverride asserts whether the resolver surfaced the override
		// value in the result (nil pointer = clean inherit, non-nil = the
		// row was present even if it ultimately fell through because it
		// was garbled). Lets us distinguish "row missing, fell through"
		// from "row present but garbled, fell through" — both resolve to
		// the same mode but the resolver tells operators which case it was.
		hasOverride bool
	}
	type tc struct {
		name        string
		workspaceID string
		orgMode     string
		setupMock   func(m sqlmock.Sqlmock)
		want        want
		wantErr     bool
	}

	cases := []tc{
		{
			name:        "workspace_override_byok_overrides_pm_org",
			workspaceID: wsID,
			orgMode:     LLMBillingModePlatformManaged,
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceWorkspaceOverride, hasOverride: true},
		},
		{
			name:        "workspace_override_disabled_overrides_pm_org",
			workspaceID: wsID,
			orgMode:     LLMBillingModePlatformManaged,
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeDisabled))
			},
			want: want{mode: LLMBillingModeDisabled, source: BillingModeSourceWorkspaceOverride, hasOverride: true},
		},
		{
			name:        "workspace_override_null_inherits_byok_org",
			workspaceID: wsID,
			orgMode:     LLMBillingModeBYOK,
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceOrgDefault, hasOverride: false},
		},
		{
			name:        "workspace_override_null_inherits_pm_org",
			workspaceID: wsID,
			orgMode:     LLMBillingModePlatformManaged,
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceOrgDefault, hasOverride: false},
		},
		{
			name:        "workspace_override_garbled_falls_through_to_pm_org_DEFAULT_CLOSED",
			workspaceID: wsID,
			orgMode:     LLMBillingModePlatformManaged,
			setupMock: func(m sqlmock.Sqlmock) {
				// CHECK constraint would normally prevent this but if a future
				// migration loosens it (or a direct UPDATE bypasses it on a
				// non-PG driver in a test stub), a garbled value MUST NOT
				// be honored as if it were valid. This is the default-closed
				// safety axis the RFC calls out.
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("byokk"))
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceOrgDefault, hasOverride: true},
		},
		{
			name:        "workspace_override_garbled_org_garbled_constant_fallback",
			workspaceID: wsID,
			orgMode:     "garbled-or-empty",
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow("nonsense"))
			},
			// Both layers garbled → constant fallback. Source is constant_fallback
			// so operators can see the org-default-was-also-bad case explicitly.
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceConstantFallback, hasOverride: true},
		},
		{
			name:        "workspace_row_missing_falls_through_to_org_byok",
			workspaceID: wsID,
			orgMode:     LLMBillingModeBYOK,
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}))
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceOrgDefault, hasOverride: false},
		},
		{
			name:        "workspace_id_empty_pre_provision_org_only",
			workspaceID: "",
			orgMode:     LLMBillingModeBYOK,
			setupMock:   func(m sqlmock.Sqlmock) { /* no DB read expected — empty ws id short-circuits */ },
			want:        want{mode: LLMBillingModeBYOK, source: BillingModeSourceOrgDefault, hasOverride: false},
		},
		{
			name:        "workspace_id_empty_org_garbled_constant_fallback",
			workspaceID: "",
			orgMode:     "",
			setupMock:   func(m sqlmock.Sqlmock) { /* no DB read */ },
			want:        want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceConstantFallback, hasOverride: false},
		},
		{
			name:        "db_error_default_closed_to_pm_with_error",
			workspaceID: wsID,
			orgMode:     LLMBillingModeBYOK, // org says byok but DB errored — DO NOT honor org
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnError(errors.New("connection refused"))
			},
			// Critical: even though orgMode=byok, a DB error means we can't
			// confirm the workspace doesn't have an override, so we default
			// to the closed mode. This is the safer of the two failures —
			// silently flipping to org-byok on a DB error would leak the
			// OAuth-keeping behavior to workspaces whose row says NULL.
			want:    want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceConstantFallback, hasOverride: false},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock := setupTestDB(t)
			c.setupMock(mock)

			res, err := ResolveLLMBillingMode(ctx, c.workspaceID, c.orgMode)
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
				t.Errorf("hasOverride: got %v want %v (override=%v)",
					res.WorkspaceOverride != nil, c.want.hasOverride, res.WorkspaceOverride)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid asserts the resolver's
// post-condition: the returned mode is ALWAYS one of the three known enum
// values, never an empty string and never a garbled passthrough. The strip
// gate downstream relies on this so it can switch on res.ResolvedMode
// without a separate is-valid check on every call site.
func TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid(t *testing.T) {
	ctx := context.Background()
	const wsID = "22222222-2222-2222-2222-222222222222"

	// Throw a pathological row at the resolver: garbled override + garbled
	// org default. Resolved mode must still be a recognized enum.
	mock := setupTestDB(t)
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
		t.Errorf("default-closed contract: garbled-x-garbled must resolve to platform_managed, got %q", res.ResolvedMode)
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
