package handlers

// Behavioral regression test for the #4147 restarting-agent path.
//
// WHY THIS FILE EXISTS — the miss it is closing:
//
// #4151 shipped with unit tests that pinned the CLASSIFICATION
// (isAgentRestartingError maps ECONNREFUSED to workspace_settling, disjoint from
// busy_retryable) but NOTHING that pinned the BEHAVIOUR of the path the traffic
// was newly routed into. The ephemeral gate went green via the RETRY path, so
// which path fired was a race and the enqueue path was never actually exercised
// end to end. Staging then hit it (run 486390): the A2A ping was queued, drained
// into the freshly-restarted agent's BOOT TURN, and answered with
//
//   "Workspace restarted and ready. LLM_PROVIDER and MODEL env vars are now
//    available. What would you like me to help with?"
//
// instead of the requested PONG. Silently wrong content — worse than a loud
// retry, because the caller cannot tell it was never answered.
//
// Enqueue is correct for a BUSY agent: heartbeat-gated drain (ActiveTasks <
// maxConcurrent) only fires once its turn ends, so the item lands on an idle
// agent. A RESTARTING agent reports ActiveTasks=0 while still producing its own
// first turn, which breaks that invariant.
//
// So: a restarting agent must take the retryable 503 ("workspace agent
// restarting"), which A2A callers already treat as a bounded-retry settling
// class. That was all #4147 ever needed — the ORIGINAL bug was that ECONNREFUSED
// surfaced as {"error":"failed to reach workspace agent"}, a body matching no
// retryable pattern, so callers gave up after one attempt.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// A restarting agent (dial REFUSED, container verifiably alive) must answer with
// a retryable 503 whose body names the settling class — and must NOT be enqueued.
func TestProxyA2A_RestartingAgent_Returns503AndIsNotQueued(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()

	// A non-nil provisioner makes HasProvisioner() true, so
	// containerLivenessIsVerifiable() can return true and the restarting branch
	// becomes reachable at all. With a nil provisioner the handler cannot tell
	// "restarting" from "dead" and correctly falls back to the honest 502
	// (that is TestProxyA2A_AgentUnreachable, which must keep passing).
	handler := NewWorkspaceHandler(broadcaster, &provisioner.Provisioner{}, "http://localhost:8080", t.TempDir())

	// Pin the ambient "am I inside a container?" detection OFF.
	//
	// platformInDocker is detected from /.dockerenv (a2a_proxy.go:57). When true,
	// the proxy REWRITES a stored loopback agent URL into the Docker-DNS form
	// http://ws-<id>:8000 — correct in production, fatal to this test: that name
	// does not resolve, so the dial fails with "no such host" (a DNS error), not
	// "connection refused" (ECONNREFUSED). isAgentRestartingError only recognises
	// the latter, so the restarting branch never runs and the handler correctly
	// falls through to the plain 502.
	//
	// That is a LOCAL-vs-CI trap, and it bit exactly that way: tests on my laptop
	// run on the host (no /.dockerenv → no rewrite → port 1 refuses → PASS), while
	// CI runs them INSIDE a container (/.dockerenv → rewrite → DNS failure → 502 →
	// FAIL). Leaving this to the ambient environment makes the test's meaning
	// depend on where it runs. Pin it.
	defer setPlatformInDockerForTest(false)()

	const wsID = "ws-restarting"
	// Port 1 refuses the connection => ECONNREFUSED => isAgentRestartingError.
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), "http://127.0.0.1:1")

	// ProxyA2A issues several lookups (delivery_mode, runtime, budget, activity
	// logging) whose order is an implementation detail. Match unordered so this
	// test pins the RESPONSE, not the query sequence — otherwise it goes green
	// for the wrong reason: an unmatched COALESCE(runtime) query makes
	// containerLivenessIsVerifiable() fail-closed to false, the restarting branch
	// never runs, and the assertions below would be exercising the ordinary 502
	// path instead of the one under test.
	mock.MatchExpectationsInOrder(false)
	expectBudgetCheck(mock, wsID)

	// The runtime lookup has SEVERAL consumers on this path — maybeMarkContainerDead
	// (a2a_proxy_helpers.go:366) reads it, containerLivenessIsVerifiable reads it
	// again — and each call consumes one expectation. Registering an exact count is
	// a trap, and it bit: pinned at 2 this passed on my laptop and FAILED in CI with
	// 502, because CI drove one extra read. When the expectations run out the query
	// returns "all expectations were already fulfilled", containerLivenessIsVerifiable
	// fails CLOSED to false, the restarting branch never runs, and the test silently
	// exercises the plain 502 path instead of the one under test.
	//
	// So: register it generously. HOW MANY times the handler looks up the runtime is
	// an implementation detail; this test pins the RESPONSE. Surplus expectations are
	// harmless — nothing here asserts ExpectationsWereMet.
	//
	// A non-external runtime means we own the compute, so a refused dial really
	// is a restart window rather than a dead agent.
	for i := 0; i < 8; i++ {
		mock.ExpectQuery("SELECT COALESCE\\(runtime").
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))
	}

	// Failure activity log for the settling response.
	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Restarting Agent"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Stub the enqueue so it SUCCEEDS. This is what makes the test a real guard
	// rather than a tautology: without it, a regression that re-enables the
	// enqueue would try to INSERT, find no mock expectation, FAIL the insert, and
	// fall through to the very same 503 fallback — so the assertions below would
	// still pass and prove nothing. With a succeeding stub, a regression returns
	// 202 + queue_id and is caught.
	enqueueCalled := false
	handler.enqueueA2A = func(_ context.Context, _, _ string, _ int, _ []byte, _, _ string, _ *time.Time) (string, int, error) {
		enqueueCalled = true
		return "q-should-not-happen", 1, nil
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"reply with PONG"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	proxyA2AAuthenticatedForTest(handler, c)
	time.Sleep(100 * time.Millisecond)

	// The sharpest assertion: a restarting agent must never reach the queue at all.
	if enqueueCalled {
		t.Fatalf("a RESTARTING agent was enqueued — the drain lands the message in the agent's boot turn and returns THAT turn's text as the reply (staging run 486390 answered a greeting instead of PONG). Restarting must take the retryable 503 instead. Body: %s", w.Body.String())
	}

	// THE REGRESSION GUARD. 202 means the message went into the queue, where the
	// drain would land it in the agent's boot turn and return that turn's text as
	// the answer. That is the staging failure; it must never come back.
	if w.Code == http.StatusAccepted {
		t.Fatalf("restarting agent was ENQUEUED (202) — the drain lands the message in the agent's boot turn and returns THAT turn's output as the reply (staging run 486390 got a greeting instead of PONG). It must take the retryable 503 instead. Body: %s", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (retryable settling), got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	if _, queued := resp["queue_id"]; queued {
		t.Errorf("response carries a queue_id — the request was enqueued: %s", w.Body.String())
	}

	// The body string is load-bearing, not cosmetic: A2A callers decide whether
	// to retry by MATCHING it (tests/e2e/test_staging_full_saas.sh retries on
	// /workspace agent unreachable|connection refused|restarting|.../). The
	// original #4147 bug was precisely a body — "failed to reach workspace
	// agent" — that matched none of them, so callers gave up after one attempt.
	errMsg, _ := resp["error"].(string)
	if !containsFold(errMsg, "restarting") {
		t.Errorf("error body %q does not contain \"restarting\" — A2A callers match on that substring to decide the request is retryable; without it they give up after ONE attempt, which is the original #4147 bug", errMsg)
	}

}

func containsFold(haystack, needle string) bool {
	h := []rune(haystack)
	n := []rune(needle)
	if len(n) == 0 {
		return true
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
