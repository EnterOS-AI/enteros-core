package handlers

// schedules_createrace_test.go — the #4448 create-race fix (P4b reader-removal):
// Create arms the scheduler daemon SYNCHRONOUSLY before forwarding to the volume
// grid (a 2xx reload proves /internal/schedules is serving), and createVolume
// retries a TRANSIENT dial failure a bounded number of times, surfacing a
// retryable 503 rather than a bare 502. A completed HTTP round-trip (incl. a
// real 4xx) is never retried. All cases are mutation-oriented: each asserts a
// property that flips if the specific guard is removed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// raceResp scripts one POST /internal/schedules attempt. dialFail hijacks +
// closes the connection to force a transport error in the core client (the
// transient case the retry rides out); otherwise the stub replies status+body
// (empty body → echo the posted entry as the created 201).
type raceResp struct {
	dialFail bool
	status   int
	body     string
}

type createRaceStub struct {
	mu           sync.Mutex
	requests     []string
	postCalls    int
	script       []raceResp
	reloadStatus int
	grid         string // GET /internal/schedules body (the stored grid)
	srv          *httptest.Server
}

// newCreateRaceStub serves POST /internal/daemons/reload (reloadStatus, default
// 200), GET /internal/schedules (the grid, default empty), and POST
// /internal/schedules (scripted per attempt; default 201-echo), recording order.
func newCreateRaceStub(t *testing.T, script []raceResp) *createRaceStub {
	t.Helper()
	s := &createRaceStub{script: script, reloadStatus: http.StatusOK, grid: `{"schedules":[]}`}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/daemons/reload":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.reloadStatus)
			fmt.Fprint(w, "{}")
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules":
			s.mu.Lock()
			g := s.grid
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, g)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/schedules":
			s.mu.Lock()
			i := s.postCalls
			s.postCalls++
			s.mu.Unlock()
			resp := raceResp{status: http.StatusCreated}
			if i < len(s.script) {
				resp = s.script[i]
			}
			if resp.dialFail {
				// Force a transport error: hijack the conn and close it without a
				// response, so the core client's Do() returns err != nil. Fail
				// LOUDLY if the writer can't hijack (e.g. an HTTP/2 test server),
				// rather than silently degrading to an empty-200 that would flip
				// the retry/duplicate assertions to the wrong branch.
				hj, ok := w.(http.Hijacker)
				if !ok {
					panic("createRaceStub: ResponseWriter is not http.Hijacker — dialFail cannot force a transport error")
				}
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.status)
			if resp.body != "" {
				fmt.Fprint(w, resp.body)
				return
			}
			var e volumeEntry
			_ = json.NewDecoder(r.Body).Decode(&e)
			e.Source = "runtime"
			b, _ := json.Marshal(e)
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such route"}`)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *createRaceStub) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *createRaceStub) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.postCalls
}

func firstIndexOf(reqs []string, want string) int {
	for i, r := range reqs {
		if r == want {
			return i
		}
	}
	return -1
}

// doScheduleCreate drives Schedules.Create for wsID with a JSON body.
func doScheduleCreate(t *testing.T, h *ScheduleHandler, wsID, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader([]byte(jsonBody)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	h.Create(c)
	return w
}

const createRaceBody = `{"name":"nightly","cron_expr":"0 3 * * *","timezone":"UTC","prompt":"p","enabled":true}`

// expectCreateArmAndResolve wires the DB mocks a Create makes up to the forwards:
// declare INSERT, then the arm's 1-col url + inbound secret (cached), then
// createVolume's 2-col url (secret reused from cache — no 2nd secret query).
func expectCreateArmAndResolve(mock sqlmock.Sqlmock, wsID, url, secret string) {
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectFanoutURL(mock, wsID, url)
	expectInboundSecret(mock, wsID, secret)
	expectURL(mock, wsID, url)
}

// TestScheduleCreate_ArmsSynchronouslyBeforeForward is the core of the #4448 fix:
// the daemon reload (arm) must be forwarded BEFORE the create POST, so createVolume
// never races a still-booting runtime.
//
// The stub REQUEST-ORDER assertion is the load-bearing check — DB expectations are
// matched UNORDERED (MatchExpectationsInOrder(false)) precisely so that reordering
// arm vs createVolume does NOT trip sqlmock first; the only thing that fails is the
// reloadIdx<createIdx assertion below. Mutation-verified: running h.createVolume
// before the arm makes the create POST land first and this test fails HERE (not on
// a mock-ordering error, which would have made the assertion vacuous — the #112
// class the review flagged).
func TestScheduleCreate_ArmsSynchronouslyBeforeForward(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0001-4000-8000-000000000001"
	stub := newCreateRaceStub(t, nil) // reload 200, create 201
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-1")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	reqs := stub.got()
	reloadIdx := firstIndexOf(reqs, "POST /internal/daemons/reload")
	createIdx := firstIndexOf(reqs, "POST /internal/schedules")
	if reloadIdx < 0 || createIdx < 0 || reloadIdx > createIdx {
		t.Errorf("arm must precede create (synchronous arm): reload@%d create@%d reqs=%v", reloadIdx, createIdx, reqs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestScheduleCreate_RetriesTransientDialFailure — a transient dial failure on the
// first create forward is retried; the second attempt succeeds → 201. Proves the
// bounded retry rides out a brief runtime-readiness gap.
func TestScheduleCreate_RetriesTransientDialFailure(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0002-4000-8000-000000000002"
	stub := newCreateRaceStub(t, []raceResp{{dialFail: true}}) // attempt 0 dial-fails, attempt 1 → 201
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-2")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("transient dial failure must be retried to success, got %d: %s", w.Code, w.Body.String())
	}
	if n := stub.postCount(); n != 2 {
		t.Errorf("want exactly 2 create attempts (1 retry), got %d", n)
	}
}

// TestScheduleCreate_TerminalUnreachable_Returns503 — if the runtime never
// answers (all attempts dial-fail), Create returns a RETRYABLE 503 "scheduler
// starting, retry", not a bare 502. Server is closed so every forward dial-fails.
func TestScheduleCreate_TerminalUnreachable_Returns503(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0003-4000-8000-000000000003"
	stub := newCreateRaceStub(t, nil)
	deadURL := stub.srv.URL
	stub.srv.Close() // every forward now dials a dead listener
	expectCreateArmAndResolve(mock, wsID, deadURL, "cr-secret-3")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal unreachable must be 503, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("scheduler starting")) {
		t.Errorf("503 body should say 'scheduler starting, retry', got %s", body)
	}
}

// TestScheduleCreate_RealBadRequest_NotRetried — a real 4xx from the runtime (a
// completed round-trip, err==nil) is relayed WITHOUT retry. Guards the retry
// scope: only transient dial failures retry, never a real runtime rejection.
func TestScheduleCreate_RealBadRequest_NotRetried(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0004-4000-8000-000000000004"
	stub := newCreateRaceStub(t, []raceResp{{status: http.StatusBadRequest, body: `{"error":"invalid cron"}`}})
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-4")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("real 4xx must be relayed as 400, got %d: %s", w.Code, w.Body.String())
	}
	if n := stub.postCount(); n != 1 {
		t.Errorf("a real 4xx must NOT be retried, got %d create attempts", n)
	}
}

// TestScheduleCreate_RetriesGatewayNotReady — a COMPLETED 503 (err==nil) from an
// ingress whose upstream runtime is still booting is a transient not-ready signal
// and must be RETRIED (not relayed as the bare gateway error #4448 targets). The
// review flagged that dial-failure-only retry misses the real deploy topology.
func TestScheduleCreate_RetriesGatewayNotReady(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0008-4000-8000-000000000008"
	// attempt 0 → 503 (ingress upstream booting), attempt 1 → 201.
	stub := newCreateRaceStub(t, []raceResp{{status: http.StatusServiceUnavailable, body: `{"error":"upstream not ready"}`}})
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-8")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("a transient gateway 503 must be retried to success, got %d: %s", w.Code, w.Body.String())
	}
	if n := stub.postCount(); n != 2 {
		t.Errorf("want 2 create attempts (503 retried), got %d", n)
	}
}

// TestScheduleCreate_DuplicateAfterRetry_TreatedAsSuccess — the idempotency
// guard: if a prior attempt actually landed the create on the runtime but its
// response was lost to a transient error, the retry hits the store's name guard
// and gets 400 "already exists". On a RETRY that is a false failure — the
// schedule exists — so Create returns 201, not the relayed 400.
func TestScheduleCreate_DuplicateAfterRetry_TreatedAsSuccess(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0005-4000-8000-000000000005"
	stub := newCreateRaceStub(t, []raceResp{
		{dialFail: true}, // attempt 0: landed on runtime, response lost
		{status: http.StatusBadRequest, body: `{"error":"schedule already exists: nightly"}`},
	})
	// The GET createVolume makes to confirm the already-exists entry finds a grid
	// row that MATCHES the request → the landed-then-lost replay is masked as 201.
	stub.grid = `{"schedules":[{"name":"nightly","cron":"0 3 * * *","timezone":"UTC","prompt":"p","enabled":true,"source":"runtime"}]}`
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-5")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("400 'already exists' on a RETRY (grid matches) must be success (201), got %d: %s", w.Code, w.Body.String())
	}
	if n := stub.postCount(); n != 2 {
		t.Errorf("want 2 attempts, got %d", n)
	}
}

// TestScheduleCreate_DuplicateAfterRetry_DifferentSchedule_Relayed — the [6]
// guard: a 400 "already exists" after a retry whose grid entry DIFFERS from the
// request (a genuine pre-existing conflict, not our lost create) must relay the
// 400, NOT mask a different schedule as our fresh 201.
func TestScheduleCreate_DuplicateAfterRetry_DifferentSchedule_Relayed(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0007-4000-8000-000000000007"
	stub := newCreateRaceStub(t, []raceResp{
		{dialFail: true},
		{status: http.StatusBadRequest, body: `{"error":"schedule already exists: nightly"}`},
	})
	// Pre-existing "nightly" with a DIFFERENT cron/prompt than the request →
	// gridEntryMatches is false → relay the 400.
	stub.grid = `{"schedules":[{"name":"nightly","cron":"0 9 * * *","timezone":"UTC","prompt":"other","enabled":true,"source":"runtime"}]}`
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-7")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a pre-existing DIFFERENT schedule must relay 400 (not mask), got %d: %s", w.Code, w.Body.String())
	}
}

// TestScheduleCreate_FirstAttemptDuplicate_IsRelayed — negative control for the
// idempotency guard: a 400 "already exists" on the FIRST attempt (attempt 0, no
// prior create) is a GENUINE duplicate and must be relayed as 400, NOT masked as
// success. Ensures the guard is scoped to retries only.
func TestScheduleCreate_FirstAttemptDuplicate_IsRelayed(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	wsID := "44e59f01-0006-4000-8000-000000000006"
	stub := newCreateRaceStub(t, []raceResp{
		{status: http.StatusBadRequest, body: `{"error":"schedule already exists: nightly"}`},
	})
	expectCreateArmAndResolve(mock, wsID, stub.srv.URL, "cr-secret-6")

	w := doScheduleCreate(t, h, wsID, createRaceBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a genuine first-attempt duplicate must relay 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestArmSchedulerPlugin_ReturnsReloadOutcome — armSchedulerPlugin returns true
// only on a 2xx reload (the readiness signal Create relies on), false on a non-2xx.
func TestArmSchedulerPlugin_ReturnsReloadOutcome(t *testing.T) {
	allowLoopbackForTest(t)

	t.Run("2xx reload → true", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		wsID := "44e59f01-00a1-4000-8000-0000000000a1"
		stub := newCreateRaceStub(t, nil)
		expectFanoutURL(mock, wsID, stub.srv.URL)
		expectInboundSecret(mock, wsID, "arm-secret-a")
		if !armSchedulerPlugin(context.Background(), wsID) {
			t.Errorf("arm must return true on a 2xx reload")
		}
	})

	t.Run("non-2xx reload → false", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		wsID := "44e59f01-00a2-4000-8000-0000000000a2"
		stub := newCreateRaceStub(t, nil)
		stub.reloadStatus = http.StatusInternalServerError
		expectFanoutURL(mock, wsID, stub.srv.URL)
		expectInboundSecret(mock, wsID, "arm-secret-b")
		if armSchedulerPlugin(context.Background(), wsID) {
			t.Errorf("arm must return false on a non-2xx reload")
		}
	})
}

// TestEnsureAndArmSchedulerPluginSync_DeclaresThenArms — the Create-path helper
// declares (INSERT) then arms synchronously, returning (armed, nil) on a 2xx
// reload.
func TestEnsureAndArmSchedulerPluginSync_DeclaresThenArms(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "44e59f01-00a3-4000-8000-0000000000a3"
	stub := newCreateRaceStub(t, nil)
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "arm-secret-c")

	armed, err := ensureAndArmSchedulerPluginSync(context.Background(), wsID)
	if err != nil {
		t.Fatalf("declare must succeed: %v", err)
	}
	if !armed {
		t.Errorf("want armed=true on a 2xx reload")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}
