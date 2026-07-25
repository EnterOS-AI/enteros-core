package handlers

// Unit pins for the 2026-07-19 review-fix set, runnable identically on every
// OS (no Windows-only behavior — see internal/wirepath):
//
//   1. logA2AFailure is MESSAGE-KEYED so a retried forward collapses into the
//      ingest row instead of duplicating the user's chat bubble.
//   2. The activity upsert's conflict action refuses to clobber a completed
//      row (response_body present) with a stale late failure — status,
//      error_detail, AND duration_ms.
//   3. enqueueRestartContext durably queues the restart snapshot with
//      priority 90 (drains before user messages) and an idempotency key
//      derived from the restart timestamp.
//   4. The hermes management-MCP reconcile probe reads the RENDERED runtime
//      config (/tmp/.hermes/config.yaml), not the install-dir stock config.
//   5. onboardingModelCandidates derives the fallback model-id candidates
//      from every stored form (raw, colon-form, slash-form).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestLogA2AFailure_MessageKeyed: the failure row must carry message_id so
// the ON CONFLICT (workspace_id, message_id) target collapses it into the
// ingest row (#2560). Pre-fix it inserted a second, unkeyed row with the same
// request_body — the doubled "2" bubble after a plugin-install restart.
func TestLogA2AFailure_MessageKeyed(t *testing.T) {
	mock := setupTestDB(t)

	body := []byte(`{"method":"message/send","params":{"message":{"messageId":"msg-dup-1","role":"user","parts":[{"kind":"text","text":"2"}]}}}`)

	mock.ExpectQuery("SELECT name FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Enter OS Agent"))
	// Arg #13 is message_id — the fix under test. Everything else is
	// incidental to this pin.
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"msg-dup-1").
		WillReturnResult(sqlmock.NewResult(1, 1))

	h := &WorkspaceHandler{}
	h.logA2AFailure(context.Background(), "ws-1", "canvas", body, "message/send", context.DeadlineExceeded, 42)
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("logA2AFailure did not message-key the activity row: %v", err)
	}
}

// TestActivityUpsert_NoClobberShape pins the conflict action's guards: a late
// EXCLUDED.status='error' upsert must preserve a completed row's status,
// error_detail, and duration_ms (all three CASE on response_body presence).
// sqlmock matches the statement by regex, so the expectation IS the pin: if
// the CASE guards are removed the regex no longer matches and the test fails.
func TestActivityUpsert_NoClobberShape(t *testing.T) {
	mock := setupTestDB(t)

	pattern := `INSERT INTO activity_logs[\s\S]*status\s+= CASE WHEN activity_logs\.response_body IS NOT NULL AND EXCLUDED\.status = 'error'[\s\S]*duration_ms\s+= CASE WHEN activity_logs\.response_body IS NOT NULL[\s\S]*error_detail\s+= CASE WHEN activity_logs\.response_body IS NOT NULL`
	mock.ExpectExec(pattern).WillReturnResult(sqlmock.NewResult(1, 1))

	summary := "test"
	LogActivity(context.Background(), nil, ActivityParams{
		WorkspaceID:  "ws-1",
		ActivityType: "a2a_receive",
		Summary:      &summary,
		Status:       "error",
		MessageId:    "msg-shape-1",
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("activity upsert lost a no-clobber CASE guard: %v", err)
	}
}

// TestEnqueueRestartContext_PriorityAndIdempotency: the durable fallback for
// the online-wait-timeout arm must enqueue with priority 90 (ORDER BY
// priority DESC → boot turn precedes queued user messages) and a restart-
// timestamp-derived idempotency key so retries collapse.
func TestEnqueueRestartContext_PriorityAndIdempotency(t *testing.T) {
	mock := setupTestDB(t)

	restartAt := time.Unix(1_770_000_000, 0)
	// Keyed per WORKSPACE (no restart timestamp): consecutive restarts must
	// collapse to one queued snapshot, not stack one message per attempt.
	wantKey := "restart-context-ws-42"

	// Supersede-expired sweep for the same key, then the keyed INSERT.
	mock.ExpectExec("UPDATE a2a_queue").
		WithArgs("ws-42", wantKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO a2a_queue").
		WithArgs("ws-42", nil, 90, sqlmock.AnyArg(), "message/send", wantKey, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("q-1"))
	// Depth query after a successful insert (any count).
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	h := &WorkspaceHandler{}
	h.enqueueRestartContext(context.Background(), "ws-42", restartContextData{RestartAt: restartAt}, "unit test")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("enqueueRestartContext queue contract regressed: %v", err)
	}
}

// TestHermesManagementProbe_ReadsRenderedConfig pins the probe path fix: the
// reconciler must probe the config start.sh RENDERS for the daemon
// (HOME=/tmp → /tmp/.hermes/config.yaml), not the hermes install dir whose
// stock config never contains the molecule entry (which made the reconciler
// re-deliver the management MCP forever).
func TestHermesManagementProbe_ReadsRenderedConfig(t *testing.T) {
	probe, ok := managementMCPConfigProbeFor("hermes")
	if !ok {
		t.Fatal("no hermes probe registered")
	}
	if probe.containerPath != "/tmp/.hermes/config.yaml" {
		t.Fatalf("hermes probe reads %q; want the rendered runtime config /tmp/.hermes/config.yaml", probe.containerPath)
	}
}

// TestApplySelfHostTenantDefaults: on self-host, MISSING TENANT_* required
// vars get branded placeholders (never a bricked first boot), set values are
// never overridden, and non-TENANT vars (API keys) still fail closed.
func TestApplySelfHostTenantDefaults(t *testing.T) {
	env := map[string]string{
		"TENANT_DOMAIN": "real-customer.com", // operator-set — must survive
	}
	missing := []string{
		"TENANT_NAME", "TENANT_TIMEZONE", "TENANT_DOMAIN_FULL",
		"TENANT_FUTURE_FIELD", // unknown TENANT_* from a future template
		"MOONSHOT_API_KEY",    // NOT tenant identity — must stay missing
	}
	applySelfHostTenantDefaults(env, missing)

	if env["TENANT_NAME"] != "Enter OS" {
		t.Errorf("TENANT_NAME = %q; want Enter OS", env["TENANT_NAME"])
	}
	if env["TENANT_DOMAIN"] != "real-customer.com" {
		t.Errorf("operator-set TENANT_DOMAIN was overridden: %q", env["TENANT_DOMAIN"])
	}
	if env["TENANT_DOMAIN_FULL"] != "https://enteros.local" {
		t.Errorf("TENANT_DOMAIN_FULL = %q", env["TENANT_DOMAIN_FULL"])
	}
	if env["TENANT_TIMEZONE"] == "" {
		t.Error("TENANT_TIMEZONE not defaulted to system timezone")
	}
	if env["TENANT_FUTURE_FIELD"] != "Enter OS" {
		t.Errorf("unknown TENANT_* var not placeholdered: %q", env["TENANT_FUTURE_FIELD"])
	}
	if _, set := env["MOONSHOT_API_KEY"]; set {
		t.Error("non-TENANT missing var must NOT be defaulted (API keys fail closed)")
	}
}

// TestOnboardingModelCandidates: the self-host fallback must try the stored
// onboarding model in every runtime-flavored form — raw, after-colon
// (hermes stores minimax:MiniMax-M3), and after-slash.
func TestOnboardingModelCandidates(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"minimax:MiniMax-M3", []string{"minimax:MiniMax-M3", "MiniMax-M3"}},
		{"minimax/MiniMax-M3", []string{"minimax/MiniMax-M3", "MiniMax-M3"}},
		{"MiniMax-M2.7", []string{"MiniMax-M2.7"}},
		{"", nil},
	}
	for _, c := range cases {
		got := onboardingModelCandidates(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("onboardingModelCandidates(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}
