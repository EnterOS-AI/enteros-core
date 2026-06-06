package handlers

// Phase 4b — unit coverage for the gate's activation SCOPE: the default-off
// rollout flag + org-token-only targeting. These exercise the short-circuit
// paths that return "proceed" BEFORE requireApproval, so they need no DB. The
// full flag-on + org-token + gated → 202 path is covered by the real-Postgres
// approval_gate_integration_test.go.

import (
	"net/http/httptest"
	"os"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/approvals"
	"github.com/gin-gonic/gin"
)

func TestDestructiveGateEnabled_DefaultOff(t *testing.T) {
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	if destructiveGateEnabled() {
		t.Fatal("gate must be OFF by default (no env)")
	}
	for _, v := range []string{"1", "true"} {
		t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", v)
		if !destructiveGateEnabled() {
			t.Errorf("%q must enable the gate", v)
		}
	}
	t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", "0")
	if destructiveGateEnabled() {
		t.Error(`"0" must keep the gate off`)
	}
}

func TestCallerHoldsOrgToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if callerHoldsOrgToken(c) {
		t.Error("no org_token_id in context → must be false (workspace/CP caller)")
	}
	c.Set("org_token_id", "tok-abc")
	if !callerHoldsOrgToken(c) {
		t.Error("org_token_id set → must be true (platform-agent / org-admin caller)")
	}
}

// gateDestructive must return true (proceed, no 202, no DB touch) whenever the
// scope excludes the call: non-gated action, flag off, or non-org-token caller.
func TestGateDestructive_ScopeShortCircuits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(orgToken bool) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("DELETE", "/x", nil)
		if orgToken {
			c.Set("org_token_id", "tok")
		}
		return c
	}

	// flag OFF (default) + org-token + gated action → proceed.
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	if !gateDestructive(newCtx(true), nil, "ws", approvals.ActionDeleteWorkspace, "r", nil) {
		t.Error("flag off must proceed (gate dormant)")
	}

	// flag ON + NO org token (workspace agent / human CP session) → proceed.
	t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", "1")
	if !gateDestructive(newCtx(false), nil, "ws", approvals.ActionDeleteWorkspace, "r", nil) {
		t.Error("non-org-token caller must proceed (normal operation unchanged)")
	}

	// flag ON + org token + NON-gated action → proceed (IsGated short-circuit).
	if !gateDestructive(newCtx(true), nil, "ws", approvals.Action("not_a_gated_action"), "r", nil) {
		t.Error("non-gated action must proceed")
	}
}
