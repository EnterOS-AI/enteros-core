package staginge2e

// platform_agent_mgmt_mcp_gate.go — the PURE, deterministic decision logic for
// Guard B: the "fresh-org platform-agent-CALLABLE" hard prod gate.
//
// WHY A SEPARATE, UNTAGGED FILE (mirrors orginstancestatus.go):
//
//	The live assertion lives in platform_agent_mgmt_mcp_e2e_test.go behind the
//	`staging_e2e` build tag (it needs a real staging tenant). But the DECISION —
//	"given what we observed about a fresh org's platform agent on a specific
//	deploy-candidate combo, is its management MCP genuinely CALLABLE?" — is pure
//	data → verdict. Extracting it here lets the verdict be unit-tested in the
//	normal `go test ./...` gate (platform_agent_mgmt_mcp_gate_test.go), including
//	a DETERMINISTIC fail-before/green proof against the exact openclaw pins that
//	regressed (broken d3f8d15 → RED, fixed cce7639 → GREEN), with NO live tenant.
//	The live test then FEEDS its observations into EvaluateMgmtMCPCallable, so the
//	gate that runs in the deploy path and the gate the unit test proves are the
//	SAME code — not two drifting copies.
//
// WHAT IT ENCODES (the philosophy, as a hard gate): a fresh org's concierge is
// only "OK to fan out to the fleet" when ALL hold on the deploy-candidate combo:
//   - it is on the EXPECTED default runtime (openclaw) — catches the default-flip
//     skew that put a fresh org on a stale/other runtime (task#225/#226/test123);
//   - it reached status=online — RCA #2970 makes online UNREACHABLE unless the
//     management MCP is present (fail-closed heartbeat);
//   - when the row surfaces them, mcp_server_present=true AND loaded_mcp_tools
//     carries the provision_workspace verb (present, not a stub);
//   - AND, crucially, a REAL A2A tool-use turn drove the concierge to actually
//     RUN provision_workspace and the requested workspace APPEARED — i.e. the verb
//     is genuinely CALLABLE, not merely present. Presence-only is exactly the
//     "checks presence/no-op-text not callability" flaw that let the mgmt-MCP /
//     presign regressions reach prod uncaught.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MgmtMCPProbe is everything the live gate OBSERVED about a fresh org's platform
// agent (concierge) on the deploy-candidate combo. It is the sole input to the
// verdict, so the verdict is fully reproducible in a unit test.
type MgmtMCPProbe struct {
	// ExpectedRuntime is the platform default runtime the fresh concierge MUST be
	// on. De-hardcoded via E2E_DEFAULT_RUNTIME from the KMS SSOT
	// (MOLECULE_DEFAULT_RUNTIME); "openclaw" per the operator directive. Empty
	// disables the runtime check (local convenience only).
	ExpectedRuntime string
	// ObservedRuntime is the concierge's actual runtime as surfaced by the tenant
	// (best-effort — see observedRuntime). Empty = the tenant does not surface the
	// runtime name on the workspace API; the check then RELIES on the workflow
	// pin (the fresh org boots MOLECULE_DEFAULT_RUNTIME) rather than false-failing.
	ObservedRuntime string
	// Status is the concierge's lifecycle status. Only "online" satisfies the RCA
	// #2970 management-MCP-present online contract.
	Status string
	// MCPServerPresentReported is true when the tenant row actually carried an
	// mcp_server_present field; MCPServerPresent is its value. A concierge that is
	// online but reports present=false is a contract violation we want RED.
	MCPServerPresentReported bool
	MCPServerPresent         bool
	// LoadedTools is the concierge's loaded_mcp_tools inventory. May be empty when
	// the runtime heartbeat producer has not surfaced it — then we lean on the
	// status=online RCA #2970 gate + the CALLABLE proof below.
	LoadedTools []string
	// RequiredTool is the fully-qualified management verb id the inventory must
		// carry: mcp__<MCPServerName>__<RequiredTool> — SSOT from molecule-ai-sdk,
	// never hardcoded by the caller.
	RequiredTool string
	// AssertCallable turns the real-A2A-turn proof into a HARD requirement (the CI
	// deploy-gate path). When false (a presence-only local run) WorkerProvisioned
	// is not required — but the caller then gets a weaker guarantee, and the gate
	// says so.
	AssertCallable bool
	// WorkerProvisioned is the DETERMINISTIC side effect of the real A2A turn: the
	// concierge was driven over A2A to call provision_workspace and a genuine
	// kind='workspace' row with the requested name appeared. This is the
	// "genuinely callable" proof that presence-only checks cannot give.
	WorkerProvisioned bool
}

// EvaluateMgmtMCPCallable is the Guard B verdict: (ok, human-readable reason).
// ok=false means the deploy candidate must NOT fan out to the fleet.
//
// The ordering of checks is intentional (cheapest / most-specific failure first)
// so a red gate names the ACTUAL regression class, not a generic timeout.
func EvaluateMgmtMCPCallable(p MgmtMCPProbe) (ok bool, reason string) {
	// 1. Default-runtime skew (task#225/#226/test123). Only enforced when BOTH the
	//    expectation is set AND the tenant actually surfaced a runtime; an
	//    unobserved runtime relies on the workflow pin (documented) rather than
	//    false-failing on a field the API may not expose.
	if p.ExpectedRuntime != "" && p.ObservedRuntime != "" && p.ObservedRuntime != p.ExpectedRuntime {
		return false, fmt.Sprintf(
			"platform agent is on runtime %q but the gate expected the default %q — a default-runtime flip/skew put the fresh org on the wrong runtime (the gate must exercise the operator default, openclaw)",
			p.ObservedRuntime, p.ExpectedRuntime)
	}

	// 2. Online — RCA #2970: a kind='platform' agent cannot reach online unless its
	//    management MCP is present (heartbeat fail-closed). Not-online ⟹ mgmt-MCP
	//    absent/degraded (the test14 / broken-openclaw-pin failure mode).
	if p.Status != "online" {
		return false, fmt.Sprintf(
			"platform agent status=%q (not online) — RCA #2970 fail-closed: its management MCP never became present (the fresh-org mgmt-MCP-absent regression, e.g. a runtime image without the platform-mcp plugin)",
			nonEmpty(p.Status, "<none>"))
	}

	// 3. When surfaced, mcp_server_present must be true (online but present=false is
	//    a contract regression, not a pass).
	if p.MCPServerPresentReported && !p.MCPServerPresent {
		return false, "platform agent is online but mcp_server_present=false — RCA #2970 online contract violated (online must imply the management MCP is present)"
	}

	// 4. When the inventory is surfaced, it MUST carry the required lifecycle verb.
	//    An empty inventory is tolerated (heartbeat producer may not surface it),
	//    and the CALLABLE proof below is the real backstop.
	if p.RequiredTool != "" && len(p.LoadedTools) > 0 && !containsStr(p.LoadedTools, p.RequiredTool) {
		return false, fmt.Sprintf(
			"platform agent reports loaded_mcp_tools=[%s] but NOT the required lifecycle verb %q — the management MCP is present but provision_workspace is not loaded/callable",
			strings.Join(p.LoadedTools, ","), p.RequiredTool)
	}

	// 5. The CALLABLE proof (the whole point of Guard B): a real A2A tool-use turn
	//    must have RUN provision_workspace and produced the workspace. Presence
	//    without callability is exactly the flaw that let regressions through.
	if p.AssertCallable && !p.WorkerProvisioned {
		return false, "platform agent is online with its management MCP present, but a REAL A2A provision_workspace turn did NOT create the requested workspace — the verb is present but not genuinely CALLABLE (a presence-only gate would have false-passed here)"
	}

	if !p.AssertCallable {
		return true, "platform agent online on the expected runtime with the management MCP present (presence-only: the real A2A callable turn was NOT required on this run)"
	}
	return true, "platform agent online on the expected runtime with the management MCP present AND provision_workspace genuinely CALLABLE (a real A2A turn created the workspace)"
}

// observedRuntime best-effort extracts the concierge's runtime name from a
// GET /workspaces/:id body. The Workspace API does not (yet) surface a top-level
// `runtime`, so it also probes the runtime-declared agent_card. Returns "" when
// the runtime is not surfaced — the caller then leans on the workflow pin rather
// than false-failing (documented in MgmtMCPProbe.ObservedRuntime).
func observedRuntime(body string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	// (1) a future/top-level `runtime` string (forward-compatible if CP surfaces it).
	if raw, ok := m["runtime"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	// (2) the runtime-sent agent_card may name the runtime (agent_card.runtime /
	//     agent_card.name). Best-effort: an absent/opaque card yields "".
	if raw, ok := m["agent_card"]; ok && len(raw) > 0 {
		var card map[string]json.RawMessage
		if json.Unmarshal(raw, &card) == nil {
			for _, k := range []string{"runtime", "name"} {
				if cv, ok := card[k]; ok {
					var s string
					if json.Unmarshal(cv, &s) == nil && s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

// containsStr reports whether want is in xs (exact match). Lives here (untagged)
// so both the gate logic and the tagged live test share one definition.
func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
