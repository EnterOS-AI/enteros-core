package staginge2e

// platform_agent_mgmt_mcp_gate_test.go — the FAIL-BEFORE / GREEN-AFTER proof for
// Guard B, runnable in the normal `go test ./...` gate (NO live tenant, NO build
// tag). It feeds EvaluateMgmtMCPCallable representative observations and
// asserts the verdict discriminates them:
//
//   - a broken default-runtime pin shipped WITHOUT
//     the platform management-MCP plugin, so a fresh concierge's mgmt-MCP never
//     became present → RCA #2970 fail-closed (never online) and a real A2A
//     provision_workspace turn could not create a worker  →  gate RED.
//   - a fixed default-runtime pin: the mgmt-MCP plugin installs, the concierge
//     reaches online with provision_workspace loaded, and a real A2A turn creates
//     the workspace  →  gate GREEN.
//
// This is the deterministic encoding of the operator's "fail-before proof" ask:
// point the SAME gate logic at the broken combo → RED, at the fixed combo →
// GREEN. It also locks the class of near-misses the presence-only checks let
// through (online-but-present-false, inventory-missing-verb, default-flip skew,
// and — the whole point of Guard B — present-but-not-callable).

import (
	"strings"
	"testing"

	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
)

// requiredVerb is the SSOT management verb id
// (mcp__molecule-platform__provision_workspace) — the SAME literal the live gate
// and the server-side heartbeat matcher use. Never hardcoded.
func requiredVerb() string {
	return "mcp__" + molcontracts.MCPServerName + "__" + molcontracts.RequiredTool
}

func TestEvaluateMgmtMCPCallable_FailBeforeGreenAfter(t *testing.T) {
	const defaultRuntime = "hermes"
	tools := []string{requiredVerb(), "mcp__molecule-platform__list_workspaces"}

	cases := []struct {
		name    string
		probe   MgmtMCPProbe
		wantOK  bool
		wantSub string // substring the reason must contain (regression class)
	}{
		{
			// THE fail-before case: the broken default-runtime pin. mgmt-MCP never
			// installed → concierge never online → no callable turn possible.
			name: "RED_broken_default_runtime_pin_never_online",
			probe: MgmtMCPProbe{
				ExpectedRuntime:   defaultRuntime,
				ObservedRuntime:   defaultRuntime,
				Status:            "degraded", // RCA #2970 fail-closed: mgmt-MCP absent
				RequiredTool:      requiredVerb(),
				AssertCallable:    true,
				WorkerProvisioned: false,
			},
			wantOK:  false,
			wantSub: "not online",
		},
		{
			// Same broken pin, alternate surfacing: it flips online but the row
			// honestly reports mcp_server_present=false. Still RED.
			name: "RED_broken_pin_online_but_present_false",
			probe: MgmtMCPProbe{
				ExpectedRuntime:          defaultRuntime,
				ObservedRuntime:          defaultRuntime,
				Status:                   "online",
				MCPServerPresentReported: true,
				MCPServerPresent:         false,
				RequiredTool:             requiredVerb(),
				AssertCallable:           true,
			},
			wantOK:  false,
			wantSub: "mcp_server_present=false",
		},
		{
			// Broken pin, third surfacing: inventory present but missing the verb.
			name: "RED_inventory_missing_provision_workspace",
			probe: MgmtMCPProbe{
				ExpectedRuntime:          defaultRuntime,
				ObservedRuntime:          defaultRuntime,
				Status:                   "online",
				MCPServerPresentReported: true,
				MCPServerPresent:         true,
				LoadedTools:              []string{"mcp__molecule-platform__list_workspaces"},
				RequiredTool:             requiredVerb(),
				AssertCallable:           true,
				WorkerProvisioned:        true,
			},
			wantOK:  false,
			wantSub: "required lifecycle verb",
		},
		{
			// THE Guard-B differentiator: everything PRESENT (online, present=true,
			// verb in the inventory) but the real A2A turn did NOT create the
			// workspace. A presence-only gate false-passes here; Guard B REDs.
			name: "RED_present_but_not_callable",
			probe: MgmtMCPProbe{
				ExpectedRuntime:          defaultRuntime,
				ObservedRuntime:          defaultRuntime,
				Status:                   "online",
				MCPServerPresentReported: true,
				MCPServerPresent:         true,
				LoadedTools:              tools,
				RequiredTool:             requiredVerb(),
				AssertCallable:           true,
				WorkerProvisioned:        false, // real A2A turn produced nothing
			},
			wantOK:  false,
			wantSub: "not genuinely CALLABLE",
		},
		{
			// Default-flip skew: the fresh org came up on claude-code instead of
			// the operator default. RED even though mgmt-MCP is otherwise healthy
			// — the gate must exercise the DEFAULT.
			name: "RED_default_runtime_flip_skew",
			probe: MgmtMCPProbe{
				ExpectedRuntime:          defaultRuntime,
				ObservedRuntime:          "claude-code",
				Status:                   "online",
				MCPServerPresentReported: true,
				MCPServerPresent:         true,
				LoadedTools:              tools,
				RequiredTool:             requiredVerb(),
				AssertCallable:           true,
				WorkerProvisioned:        true,
			},
			wantOK:  false,
			wantSub: "wrong runtime",
		},
		{
			// THE green-after case: the fixed default-runtime pin. Online on the
			// default runtime, present=true, verb loaded, AND a real A2A turn
			// created the workspace → genuinely callable → GREEN.
			name: "GREEN_fixed_default_runtime_pin_callable",
			probe: MgmtMCPProbe{
				ExpectedRuntime:          defaultRuntime,
				ObservedRuntime:          defaultRuntime,
				Status:                   "online",
				MCPServerPresentReported: true,
				MCPServerPresent:         true,
				LoadedTools:              tools,
				RequiredTool:             requiredVerb(),
				AssertCallable:           true,
				WorkerProvisioned:        true,
			},
			wantOK:  true,
			wantSub: "genuinely CALLABLE",
		},
		{
			// GREEN even when the runtime heartbeat producer has not surfaced the
			// inventory/present fields yet (empty LoadedTools, present unreported):
			// online + a real A2A callable turn is sufficient. This keeps the gate
			// from false-failing on a runtime that simply doesn't emit the inventory.
			name: "GREEN_callable_without_inventory_surfacing",
			probe: MgmtMCPProbe{
				ExpectedRuntime:   defaultRuntime,
				ObservedRuntime:   "", // tenant didn't surface the runtime name
				Status:            "online",
				RequiredTool:      requiredVerb(),
				AssertCallable:    true,
				WorkerProvisioned: true,
			},
			wantOK:  true,
			wantSub: "genuinely CALLABLE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := EvaluateMgmtMCPCallable(tc.probe)
			if ok != tc.wantOK {
				t.Fatalf("EvaluateMgmtMCPCallable ok=%v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if tc.wantSub != "" && !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q does not contain expected substring %q", reason, tc.wantSub)
			}
		})
	}
}

func TestObservedRuntime(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"top_level_runtime", `{"id":"x","runtime":"openclaw"}`, "openclaw"},
		{"agent_card_runtime", `{"id":"x","agent_card":{"runtime":"openclaw"}}`, "openclaw"},
		{"agent_card_name", `{"id":"x","agent_card":{"name":"claude-code"}}`, "claude-code"},
		{"absent", `{"id":"x","status":"online"}`, ""},
		{"null_card", `{"id":"x","agent_card":null}`, ""},
		{"not_json", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observedRuntime(tc.body); got != tc.want {
				t.Fatalf("observedRuntime(%s)=%q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gate 1.5 REQUIRE-LIVE posture (GUARD_B_REQUIRE_CALLABLE)
// ---------------------------------------------------------------------------
//
// The k8s design doc discharges Gate 1.5 by an OPERATOR running Guard B by hand
// against the controlplane-test CP, and its acceptance bar is "a fresh org
// provisions onto k8s AND its concierge answers a real provision_workspace call
// — NOT a PONG". Nothing on that hand path sets E2E_ASSERT_MGMT_MCP_CALLABLE
// (only the staging-tenant-cd deploy job does), so before this guard the verdict
// returned ok=true on the presence-only branch: a GREEN that never made the tool
// call. These cases lock that hole shut and prove the guard is not inert.

func TestEvaluateMgmtMCPCallable_RequireCallableRefusesPresenceOnly(t *testing.T) {
	const defaultRuntime = "hermes"
	tools := []string{requiredVerb()}

	// The exact probe an operator would produce on the Gate-1.5 hand path with
	// GUARD_B_REQUIRE_CALLABLE unset: a perfectly healthy fresh concierge that was
	// never asked to run the tool. Every observation-based check passes.
	presenceOnly := MgmtMCPProbe{
		ExpectedRuntime:          defaultRuntime,
		ObservedRuntime:          defaultRuntime,
		Status:                   "online",
		MCPServerPresent:         true,
		MCPServerPresentReported: true,
		LoadedTools:              tools,
		RequiredTool:             requiredVerb(),
		AssertCallable:           false,
		WorkerProvisioned:        false,
	}

	// FAIL-BEFORE (the historical behaviour this guard changes): without the
	// require-live posture the SAME probe is a green. Asserting it here documents
	// that the new RED below is caused by RequireCallable and nothing else.
	if ok, reason := EvaluateMgmtMCPCallable(presenceOnly); !ok {
		t.Fatalf("baseline: presence-only probe should still be OK when RequireCallable is unset (got RED: %s)", reason)
	} else if !strings.Contains(reason, "presence-only") {
		t.Fatalf("baseline reason %q should name the presence-only posture", reason)
	}

	// RED: same probe, require-live armed.
	req := presenceOnly
	req.RequireCallable = true
	ok, reason := EvaluateMgmtMCPCallable(req)
	if ok {
		t.Fatalf("RequireCallable=true with AssertCallable=false must be RED — a presence-only run cannot discharge Gate 1.5 (got GREEN: %s)", reason)
	}
	for _, want := range []string{"GUARD_B_REQUIRE_CALLABLE", "NOT a PONG", "misconfiguration"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("RED reason must NAME the cause; %q missing from %q", want, reason)
		}
	}

	// GREEN: require-live armed AND the real A2A turn genuinely created the
	// workspace. This is the only shape that discharges Gate 1.5.
	green := req
	green.AssertCallable = true
	green.WorkerProvisioned = true
	ok, reason = EvaluateMgmtMCPCallable(green)
	if !ok {
		t.Fatalf("require-live + a genuine callable turn must be GREEN (got RED: %s)", reason)
	}
	if !strings.Contains(reason, "genuinely CALLABLE") {
		t.Fatalf("GREEN reason %q must state the callable proof ran", reason)
	}

	// RED: require-live armed, the turn WAS armed, but the workspace never
	// appeared — the present-but-not-runnable class. Must still name callability,
	// not the require-live misconfiguration (check 0 must not shadow check 5).
	notRunnable := green
	notRunnable.WorkerProvisioned = false
	ok, reason = EvaluateMgmtMCPCallable(notRunnable)
	if ok {
		t.Fatalf("armed callable turn that produced no workspace must be RED (got GREEN: %s)", reason)
	}
	if !strings.Contains(reason, "not genuinely CALLABLE") {
		t.Fatalf("RED reason %q must name present-but-not-callable, not the require-live misconfiguration", reason)
	}
}

// TestGuardBMode locks the posture resolution: GUARD_B_REQUIRE_CALLABLE IMPLIES
// the callable turn, so an operator who sets the one Gate-1.5 variable cannot
// then receive a presence-only green; and setting only the deploy gate's
// existing variable is unchanged (add-only).
func TestGuardBMode(t *testing.T) {
	cases := []struct {
		name                    string
		assertVal, requireVal   string
		wantAssert, wantRequire bool
	}{
		{"both_unset__historical_local_default", "", "", false, false},
		{"deploy_gate_only__unchanged", "1", "", true, false},
		{"require_live_implies_assert", "", "1", true, true},
		{"both_set", "1", "1", true, true},
		{"permissive_truthy", "yes", "on", true, true},
		{"explicit_false_is_off", "0", "false", false, false},
		{"whitespace_and_case", " TRUE ", " Yes ", true, true},
		{"garbage_is_off", "maybe", "later", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, r := GuardBMode(tc.assertVal, tc.requireVal)
			if a != tc.wantAssert || r != tc.wantRequire {
				t.Fatalf("GuardBMode(%q,%q) = (assert=%v require=%v), want (assert=%v require=%v)",
					tc.assertVal, tc.requireVal, a, r, tc.wantAssert, tc.wantRequire)
			}
			if r && !a {
				t.Fatalf("invariant violated: requireCallable must imply assertCallable")
			}
		})
	}
}
