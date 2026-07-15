package handlers

// LOCAL REPRODUCTION of the staging red in run 487314 (E2E Staging SaaS, step
// 8/11: "A2A parent queue poll timed out").
//
// #4175 established the rule: a RESTARTING agent must take the retryable 503 and
// must NEVER be enqueued, because the queue drain is heartbeat-gated on
// ActiveTasks — a correct idle-signal for a BUSY agent and a wrong one for an
// agent that is still coming up.
//
// It enforced that rule for ONE transport shape: ECONNREFUSED, i.e. the listener
// is already gone. But a restart has TWO shapes, and which one you get is a race
// with the agent's teardown:
//
//	listener already down          -> dial refused -> ECONNREFUSED
//	listener accepts, then the
//	process dies mid-request       -> the connection closes with no reply -> EOF
//
// isUpstreamBusyError (a2a_proxy.go:213) matches "EOF" and "connection reset".
// So the SECOND shape is classified BUSY and enqueued — the exact behaviour
// #4175 exists to prevent, reachable by hitting the same config.yaml PUT restart
// a few milliseconds earlier. That is why staging fails nondeterministically:
// the identical code passed on run 487386 and failed on 487314.
//
// This test forces the EOF shape. It is RED on the current code (the request is
// enqueued) and is the executable statement of the bug.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// restartingListenerThatEOFs accepts the TCP connection and then closes it
// without writing a response — exactly what an agent process does when it is
// torn down after the socket is accepted but before it replies. net/http surfaces
// this to the caller as EOF.
func restartingListenerThatEOFs(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Read a little of the request, then drop the connection mid-flight.
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
			_ = c.Close()
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestProxyA2A_RestartingAgent_EOFShape_MustNotBeEnqueued(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()

	// Same pinning lesson as the ECONNREFUSED test: CI runs the suite INSIDE a
	// container, where platformInDocker rewrites a loopback agent URL to the
	// Docker-DNS form and the dial fails with "no such host" instead of the shape
	// under test. Pin it so this test means the same thing everywhere.
	defer setPlatformInDockerForTest(false)()

	handler := NewWorkspaceHandler(broadcaster, &provisioner.Provisioner{}, "http://localhost:8080", t.TempDir())

	const wsID = "ws-restart-eof"
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), restartingListenerThatEOFs(t))

	mock.MatchExpectationsInOrder(false)
	expectBudgetCheck(mock, wsID)
	for i := 0; i < 8; i++ {
		mock.ExpectQuery("SELECT COALESCE\\(runtime").
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))
	}
	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Restarting Agent"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Succeeding stub, so a regression shows up as a real 202 + queue_id rather
	// than falling through to the same 503 and passing for the wrong reason.
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

	if enqueueCalled {
		t.Fatalf("a RESTARTING agent was ENQUEUED because its restart produced EOF instead of ECONNREFUSED.\n"+
			"Both shapes are the SAME event — a config.yaml PUT restarting the agent — and which one the caller\n"+
			"sees is a race with the agent's teardown. #4175 barred the enqueue for the ECONNREFUSED shape only,\n"+
			"so the EOF shape still lands the message in a queue whose drain is heartbeat-gated and cannot fire\n"+
			"while the agent is coming up. That is staging run 487314 (\"A2A parent queue poll timed out\"), and it\n"+
			"is why the identical code passed on run 487386. Body: %s", w.Body.String())
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
	errMsg, _ := resp["error"].(string)
	if !containsFold(errMsg, "restarting") {
		t.Errorf("error body %q must contain \"restarting\" — A2A callers match that substring to decide the "+
			"request is retryable; without it they give up after one attempt", errMsg)
	}
}
