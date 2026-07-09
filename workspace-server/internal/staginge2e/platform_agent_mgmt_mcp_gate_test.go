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
