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
//   - it is on the EXPECTED default runtime (hermes) — catches the default-flip
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
	// (MOLECULE_DEFAULT_RUNTIME); empty disables the runtime check (local
	// convenience only).
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
	// RequireCallable is the REQUIRE-LIVE posture for this run: the caller has
	// declared that this invocation must satisfy the "genuinely callable" bar, so
	// a run that merely DECLINED to exercise the A2A turn (AssertCallable=false)
	// is a MISCONFIGURATION, not a weaker-but-acceptable pass.
	//
	// WHY IT EXISTS (the Gate-1.5 hole). AssertCallable is opt-in and defaults OFF
	// (E2E_ASSERT_MGMT_MCP_CALLABLE is set only by the staging-tenant-cd deploy
	// job). The k8s design doc's Gate 1.5 acceptance criterion is "a fresh org
	// provisions onto k8s AND its concierge answers a real provision_workspace
	// call — NOT a PONG", and it is discharged by an OPERATOR running Guard B by
	// hand against the controlplane-test CP. On that hand path nothing sets
	// E2E_ASSERT_MGMT_MCP_CALLABLE, so the verdict below would return ok=true on
	// the presence-only branch — a GREEN that never made the tool call the gate
	// exists to prove. RequireCallable makes that outcome RED instead.
	//
	// Mirrors the CP serving-e2e SERVING_E2E_REQUIRE_LIVE posture: under an
	// explicit require-live flag, "this arm did not actually run" is a hard
	// failure, not an optional arm.
	RequireCallable bool

	// Claim is what the concierge ITSELF said about the provision turn, parsed
	// from its real reply (see concierge_self_report_gate.go). Guard B's primary
	// evidence is and stays the row; this exists so the agent's REPORT can be
	// reconciled against that row instead of discarded. The zero value
	// (Observed=false) abstains, so every pre-existing probe is unaffected.
	Claim ConciergeClaim
	// ProvisionedWorkspaceID is the id of the row the callable turn actually
	// produced ("" when absent/not surfaced). It is the ground truth the claim's
	// published id is reconciled against.
	ProvisionedWorkspaceID string
}

// GuardBMode resolves the two Guard B posture booleans from their raw env
// values, in ONE pure place so the live test and its unit proof cannot drift.
//
//   - assertVal  = E2E_ASSERT_MGMT_MCP_CALLABLE (the deploy gate's opt-in)
//   - requireVal = GUARD_B_REQUIRE_CALLABLE     (the require-live posture)
//
// requireCallable IMPLIES assertCallable: the whole point is that an operator
// discharging Gate 1.5 sets ONE variable and cannot then receive a presence-only
// green. Setting only assertVal keeps the historical deploy-gate behaviour
// byte-for-byte (requireCallable=false), so this is add-only.
func GuardBMode(assertVal, requireVal string) (assertCallable, requireCallable bool) {
	requireCallable = isTruthyValue(requireVal)
	assertCallable = isTruthyValue(assertVal) || requireCallable
	return assertCallable, requireCallable
}

// isTruthyValue parses a permissive boolean env value (1/true/yes/on). Lives in
// this untagged file so the pure logic and the tagged live test share one
// definition.
func isTruthyValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// EvaluateMgmtMCPCallable is the Guard B verdict: (ok, human-readable reason).
// ok=false means the deploy candidate must NOT fan out to the fleet.
//
// The ordering of checks is intentional (cheapest / most-specific failure first)
// so a red gate names the ACTUAL regression class, not a generic timeout.
func EvaluateMgmtMCPCallable(p MgmtMCPProbe) (ok bool, reason string) {
	// 0. REQUIRE-LIVE (Gate 1.5). Checked FIRST and independently of every
	//    observation, because it is not a fact about the fleet — it is a fact
	//    about THIS RUN: the caller demanded the callable proof and the run did
	//    not arm it. Every check below could pass on a healthy-looking presence-
	//    only probe and hand back a green that never made a tool call, which is
	//    exactly the vacuous-pass shape this gate exists to refuse.
	if p.RequireCallable && !p.AssertCallable {
		return false, "GUARD_B_REQUIRE_CALLABLE is set but this run did NOT arm the real A2A provision_workspace turn (AssertCallable=false) — a presence-only run cannot discharge the Gate 1.5 criterion 'its concierge answers a real provision_workspace call — NOT a PONG'. This is a misconfiguration of the run, not an optional arm"
	}

	// 1. Default-runtime skew (task#225/#226/test123). Only enforced when BOTH the
	//    expectation is set AND the tenant actually surfaced a runtime; an
	//    unobserved runtime relies on the workflow pin (documented) rather than
	//    false-failing on a field the API may not expose.
	if p.ExpectedRuntime != "" && p.ObservedRuntime != "" && p.ObservedRuntime != p.ExpectedRuntime {
		return false, fmt.Sprintf(
			"platform agent is on runtime %q but the gate expected the default %q — a default-runtime flip/skew put the fresh org on the wrong runtime (the gate must exercise the operator default)",
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

	// 4b. ZERO-EVIDENCE REFUSAL (the vacuous-pass guard).
	//
	//     Checks 3 and 4 are both conditional on the tenant having SURFACED the
	//     signal they read: check 3 only fires when mcp_server_present was
	//     reported, check 4 only when loaded_mcp_tools was non-empty. That is the
	//     right tolerance individually — neither field is guaranteed by the
	//     workspace API — but taken together they leave a hole: a probe in which
	//     the tenant surfaced NEITHER signal walks past both checks and, on a
	//     presence-only run, reaches the green return below having verified
	//     nothing about the management MCP beyond status=online.
	//
	//     That is a pass:0/fail:0 verdict wearing a green coat, and it is exactly
	//     the shape this gate exists to refuse. Note the observed staging fleet
	//     ALREADY reports mcp_server_present=<absent> (the live gate logs
	//     `present=false(reported=false)` on every green run), so check 3 is a
	//     confirmed no-op in production and loaded_mcp_tools is the only positive
	//     presence signal actually being read today. If the heartbeat producer
	//     ever stops surfacing that one too, the presence half silently degrades
	//     to "online, and nothing else was asked".
	//
	//     The deploy path is unaffected: staging-tenant-cd arms
	//     E2E_ASSERT_MGMT_MCP_CALLABLE (and GUARD_B_REQUIRE_CALLABLE), so the
	//     real A2A turn in check 5 is its positive evidence and this branch
	//     cannot fire there. It closes the presence-ONLY hole, where there is no
	//     backstop at all.
	if !p.AssertCallable && !p.MCPServerPresentReported && len(p.LoadedTools) == 0 {
		return false, "platform agent reached status=online but the tenant surfaced NEITHER mcp_server_present NOR a non-empty loaded_mcp_tools inventory, and this run did not arm the real A2A callable turn — the verdict would be based on ZERO positive evidence that the management MCP exists (a vacuous pass). Re-run with E2E_ASSERT_MGMT_MCP_CALLABLE=1 (or GUARD_B_REQUIRE_CALLABLE=1) so the callable turn supplies the evidence"
	}

	// 4c. SELF-REPORT RECONCILIATION (failure mode G9 — see
	//     concierge_self_report_gate.go). Runs BEFORE check 5 on purpose: check 5
	//     reds on ANY turn that produced no workspace and says only "not
	//     genuinely CALLABLE", which reads identically whether the agent
	//     honestly answered "I don't have that tool" (G5) or asserted "Done, the
	//     workspace has been created" while creating nothing (G9). Those need
	//     different fixes, so the fabrication must be NAMED, not folded into the
	//     generic red.
	//
	//     It also catches the shape check 5 scores GREEN: the row landed under
	//     the requested NAME, and the agent published a DIFFERENT id — so every
	//     consumer of the reported id addresses a resource that does not exist.
	//
	//     This can only ADD failures. It never rescues a missing row: an absent
	//     or silent self-report abstains and check 5 still decides, which is the
	//     invariant — a self-report must never be the evidence.
	if p.AssertCallable {
		if ok, reason := ReconcileProvisionClaim(p.Claim, p.WorkerProvisioned, p.ProvisionedWorkspaceID); !ok {
			return false, reason
		}
	}

	// 5. The CALLABLE proof (the whole point of Guard B): a real A2A tool-use turn
	//    must have RUN provision_workspace and produced the workspace. Presence
	//    without callability is exactly the flaw that let regressions through.
	if p.AssertCallable && !p.WorkerProvisioned {
		// The red is unconditional — describeCallableFailure only NAMES it (see
		// callable_failure_class.go). Six materially different failures used to
		// publish this one sentence, so every investigation started by
		// re-deriving the class from raw logs that the gate had already parsed.
		return false, describeCallableFailure(callableRedBaseReason, p.Claim.Texts)
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

// ── The CREATED-ROW verdict (core#5052) ──────────────────────────────────────
//
// Guard B's callable proof asserts a DETERMINISTIC side effect: the concierge
// ran provision_workspace and a real workspace row appeared. Until core#5052
// the live test accepted that row on `name` + `kind` ALONE and never looked at
// `status`, so a workspace that was created and then FAILED to provision still
// reported "CALLABLE CONFIRMED". That is the exact vacuous-pass shape this gate
// exists to refuse — the hardest gate in the repo was scoring a row's EXISTENCE
// as proof of a working verb.
//
// The verdict below is deliberately narrow. The claim under test is "the verb
// genuinely RAN", not "the workspace booted", so a row still mid-provision is
// NOT a failure — provisioning is asynchronous and the concierge's obligation
// ends when the row is created. What must never pass is a row that reached a
// TERMINAL BAD state: `failed` (provision errored) or `removed` (rolled back).
// Those prove the create did not survive, and a gate that green-lights them is
// asserting nothing.
//
// kind is checked here too so both halves of the row verdict live in one pure,
// unit-tested place rather than being split between the gate and the live test.

// terminalBadWorkspaceStatuses are the statuses that PROVE a created workspace
// did not survive its own provision. Kept as a list (not a set literal inline)
// so the reason string can name exactly what was refused.
var terminalBadWorkspaceStatuses = []string{"failed", "removed"}

// ClassifyProvisionedWorkspace decides whether the row the concierge created in
// response to the real A2A provision_workspace turn counts as PROOF that the
// verb is genuinely callable.
//
//	kind   — the row's kind field ("" when the API did not surface it)
//	status — the row's status field ("" when the API did not surface it)
//
// An unobserved (empty) field is tolerated rather than fatal, matching the rest
// of this gate: the live test leans on the fields the tenant actually exposes
// and never false-fails on one the API may omit.
func ClassifyProvisionedWorkspace(kind, status string) (ok bool, reason string) {
	if kind != "" && kind != "workspace" {
		return false, fmt.Sprintf(
			"the concierge created a row named as requested but with kind=%q (want \"workspace\") — that is not a real team-member create, so provision_workspace did not genuinely run",
			kind)
	}
	if containsStr(terminalBadWorkspaceStatuses, status) {
		return false, fmt.Sprintf(
			"the concierge created the requested workspace but it reached TERMINAL status=%q — the row exists yet the provision did not survive, so scoring this as CALLABLE would be a vacuous pass (core#5052: the pre-fix gate matched on name+kind and never read status)",
			status)
	}
	return true, fmt.Sprintf(
		"the concierge created a real kind=%q workspace (status=%s) — provision_workspace genuinely ran",
		nonEmpty(kind, "workspace"), nonEmpty(status, "<not surfaced>"))
}

// a2aTurnLogCap bounds the A2A turn body Guard B logs. It must stay well above
// the runtime's own failure text so a red run is SELF-DIAGNOSING: hermes returns
// `Model generated invalid tool call: <tool-id>` (the id itself already capped
// at 80 chars upstream) inside a JSON-RPC message envelope of ~145 chars. The
// previous cap of 200 sliced that apart mid-identifier and left every red run
// unexplainable without a live reproduction.
const a2aTurnLogCap = 4000

// truncate bounds a body for logging, appending an ellipsis when it cuts.
//
// Lives here (untagged) so the tagged live tests and the untagged gate/unit
// tests share ONE definition — the same reason containsStr lives here. It moved
// out of the staging_e2e-tagged concierge_platform_test.go in core#5052 so
// a2aTurnLogCap could be proved against a real failure body without a live
// tenant.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
