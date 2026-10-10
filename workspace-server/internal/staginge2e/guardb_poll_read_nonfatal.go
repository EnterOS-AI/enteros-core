package staginge2e

// guardb_poll_read_nonfatal.go — the untagged half of ONE property of Guard B's
// concierge readiness wait: an UNREADABLE poll must not be able to end the gate.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE DEFECT
// ─────────────────────────────────────────────────────────────────────────────
//
// readiness_terminal_signal.go models three answers per poll, and Obs.ReadOK is
// the fourth thing it models explicitly:
//
//	// ReadOK is true when the read itself succeeded (HTTP 200 / row present).
//	// A failed read is NOT evidence of anything: it neither satisfies nor
//	// refutes readiness, so it only advances the budget.
//
// That arm is correct and unit-tested. From Guard B's LIVE call site it was
// also UNREACHABLE for the most likely way a read fails.
//
// The wait polled with doTenantJSON, which calls t.Fatalf on a transport error:
//
//	resp, err := client.Do(req)
//	if err != nil {
//	    t.Fatalf("%s %s: %v", method, url, err)
//	}
//
// So `ReadOK:false` could only ever be produced by a non-200 HTTP RESPONSE. A
// dropped connection, a TLS handshake reset, or one poll exceeding the client
// timeout did not become "no information" — it killed the test outright, from
// inside a loop that runs up to 60 times over 15 minutes against a tenant that
// was created seconds earlier and is reached over the public edge.
//
// The consequence is not cosmetic. This job is the staging deploy HARD GATE: a
// red here makes rollback-pin revert the staging tenant-image pin and reroll the
// whole staging fleet. One dropped TCP connection was sufficient to mint that
// red, and it would arrive as a bare `GET https://…: …` with no Guard B verdict,
// no status trace, and none of the CP diagnostics the wait's own failure path
// collects — i.e. the least diagnosable red the gate can produce, for the cause
// that has nothing to do with the fleet.
//
// The repo had already reasoned this out ONCE, for a strictly less important
// caller. collectConciergeSelfReport carries the note:
//
//	the read uses doTenantJSONTimeout, NOT doTenantJSON. doTenantJSON calls
//	t.Fatalf on a transport error, so a slow or flapping tenant would kill the
//	whole gate from inside a diagnostic — the exact opposite of the stated
//	contract.
//
// A diagnostic was protected; the primary readiness wait was not.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS IS NOT
// ─────────────────────────────────────────────────────────────────────────────
//
// It is NOT a retry, NOT a widened timeout, and NOT variance tolerance. No
// budget changes; conciergeOnlineBudget is untouched. The wait still ends the
// instant the real signal fires and still ends early on a PUBLISHED terminal
// verdict. All that changes is that an unreadable poll now reaches the arm that
// already existed for it, so the wait keeps watching the real signal and — if
// the tenant is unreachable for the whole budget — reports that as the STUCK
// verdict it is, with the status trace and CP diagnostics attached.
//
// HONEST SCOPE. Measured over the 261 genuine e2e-smoke Guard B verdicts in
// Gitea retention on 2026-08-09 (window 2026-07-09T13:04Z → 2026-08-09T08:21Z),
// this fatal fired ZERO times: no log in the corpus contains a transport error,
// a dial failure, a connection reset or a Client.Timeout. It is a latent hole,
// closed. It is credited with NONE of the 24 "concierge never online" or 8 "org
// never running" reds in that window, and this file does not claim otherwise.

import "time"

// conciergePollReadTimeout bounds ONE readiness poll of
// GET /workspaces/:id during Guard B's concierge wait.
//
// It is deliberately the SAME 90s doTenantJSON uses, so replacing the fatal
// reader with the non-fatal one changes nothing about how long a single poll may
// take — only what happens when it does not come back. Choosing a different
// number here would have made this a timing change wearing a correctness
// change's clothes.
//
// It must stay well under conciergeOnlineBudget: a per-poll timeout at or above
// the budget would let a single hung read consume the whole wait, which is the
// same "one poll decides everything" shape being removed. Pinned by
// TestConciergePollReadTimeoutCannotConsumeTheWholeBudget.
const conciergePollReadTimeout = 90 * time.Second
