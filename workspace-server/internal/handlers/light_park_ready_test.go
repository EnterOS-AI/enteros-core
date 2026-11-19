package handlers

// light_park_ready_test.go — a park of an agent that is being restarted
// (2026-11-18: createLightAgent's park failed because the agent's pod was
// being replaced). The park waits, with backoff, until the agent answers its
// schedule grid, reading the workspace again between tries; an agent that
// never answers is refused 409 LIGHT_AGENT_NOT_READY with Retry-After and
// nothing changed; a grid that is not a schedule list is 422
// LIGHT_SCHEDULES_UNREADABLE, never retried.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// fakeLightReadClock replaces the read loop's clock: a sleep returns at once
// and moves now on. It returns how far the clock has moved.
func fakeLightReadClock(t *testing.T) func() time.Duration {
	t.Helper()
	start := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := start
	prevNow, prevSleep := lightAgentReadNow, lightAgentReadSleep
	lightAgentReadNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	lightAgentReadSleep = func(ctx context.Context, d time.Duration) bool {
		if ctx.Err() != nil {
			return false
		}
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return true
	}
	t.Cleanup(func() { lightAgentReadNow, lightAgentReadSleep = prevNow, prevSleep })
	return func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return now.Sub(start)
	}
}

// scriptedGrid answers the park's reads in order (nil: the grid); the last
// step repeats. It counts the reads.
type scriptedGrid struct {
	mu    sync.Mutex
	steps []error
	grid  []volumeEntry
	reads int
}

func (s *scriptedGrid) read(context.Context, string) ([]volumeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.reads
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.reads++
	if err := s.steps[i]; err != nil {
		return nil, err
	}
	return s.grid, nil
}

func (s *scriptedGrid) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

var (
	errTestUnreachable = fmt.Errorf("%w: dial tcp 10.42.0.7:8000: connect: connection refused", errLightAgentUnreachable)
	errTestUnreadable  = fmt.Errorf("%w: malformed JSON: unexpected end of JSON input", errLightGridUnreadable)
)

// expectLightRecheck scripts the workspace read between two tries.
func expectLightRecheck(mock sqlmock.Sqlmock, status, state string, active int) {
	expectLightLoad(mock, status, "hermes", "workspace", state, active, nil, nil)
}

func TestParkWaitsForAnAgentThatIsRestarting(t *testing.T) {
	elapsed := fakeLightReadClock(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{
		{Name: "daily", CronJob: "light-be7540cfd160-aaaaaaaa"}, {Name: "weekly", CronJob: "light-be7540cfd160-bbbbbbbb"},
	}}}
	lh, wh, stops := newParkTestHandler(t, api, nil)
	// Two tries find no agent (its new pod is starting), the third reads the
	// grid; the read after the claim succeeds at once.
	reader := &scriptedGrid{steps: []error{errTestUnreachable, errTestUnreachable, nil}, grid: lightGrid()}
	wh.lightScheduleReader = reader.read

	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, "tok-cur", "tok-old")
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	expectLightRecheck(mock, "online", "open", 0)
	expectLightRecheck(mock, "online", "open", 0)
	expectLightClaimParking(mock, "owner", 7)
	mock.ExpectQuery(lightStoredSchedulesSQL).WithArgs(lightTestWS).WillReturnRows(lightStoredRows())
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens \(workspace_id, token_hash, prefix, kind\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-new"))
	expectLightAudit(mock)
	expectLightPendingToken(mock, "tok-new", nil)
	expectParkAfterThePut(mock, [][]driver.Value{
		{"daily", "0 9 * * *", "America/Vancouver", "Write the daily report.", true, "light-be7540cfd160-aaaaaaaa", true},
		{"weekly", "0 8 * * 1", "UTC", "Weekly review.", false, "light-be7540cfd160-bbbbbbbb", true},
	}, []string{"daily", "weekly"})
	expectHibernateThenResume(mock)

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusAccepted || lightBody(t, w)["light_state"] != "parking" {
		t.Fatalf("park = %d %s, want 202 once the agent answers", w.Code, w.Body.String())
	}
	wh.waitAsyncForTest()
	if n := reader.count(); n != 4 {
		t.Errorf("grid reads = %d, want 4 (two unanswered tries, one before the claim, one after it)", n)
	}
	if got := elapsed(); got != 3*time.Second {
		t.Errorf("waited %s, want 3s (1s, then 2s)", got)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"put " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if len(*stops) != 1 {
		t.Errorf("stops = %v, want the pod stopped once", *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// An agent that never answers: the owner's park tries for about a minute
// (1, 2, 4, 8, 8 ... seconds apart), then refuses 409 LIGHT_AGENT_NOT_READY
// with Retry-After. Nothing was claimed, written or sent: the agent is never
// 'parking' and last_error is untouched.
func TestParkOfAnAgentThatNeverAnswersIs409NotReadyAndChangesNothing(t *testing.T) {
	elapsed := fakeLightReadClock(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh, stops := newParkTestHandler(t, api, nil)
	reader := &scriptedGrid{steps: []error{errTestUnreachable}}
	wh.lightScheduleReader = reader.read

	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	// Tries at 0, 1, 3, 7, 15, 23, 31, 39, 47 and 55 s; the next would pass
	// the minute. A read of the workspace between each two.
	for i := 0; i < 9; i++ {
		expectLightRecheck(mock, "online", "open", 0)
	}
	rec := recordSQL(t)

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	wh.waitAsyncForTest()
	assertNoWrites(t, rec())
	body := lightBody(t, w)
	if w.Code != http.StatusConflict || body["code"] != "LIGHT_AGENT_NOT_READY" || body["status"] != "online" {
		t.Fatalf("park = %d %v, want 409 LIGHT_AGENT_NOT_READY with the agent's status", w.Code, body)
	}
	if got := w.Header().Get("Retry-After"); got != lightAgentNotReadyRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, lightAgentNotReadyRetryAfter)
	}
	if n := reader.count(); n != 10 {
		t.Errorf("grid reads = %d, want 10", n)
	}
	if got := elapsed(); got != 55*time.Second || got > lightAgentReadBudget {
		t.Errorf("waited %s, want 55s, inside the %s budget", got, lightAgentReadBudget)
	}
	if len(api.Calls()) != 0 || len(*stops) != 0 {
		t.Errorf("CP calls = %v, stops = %v; nothing may be sent", api.Calls(), *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (no claim may follow): %v", err)
	}
}

// The wait ends early when the workspace changes under it: an agent no longer
// online (its restart is under way), deleted, or that took a task. Each is
// the refusal the park gives before its claim, with nothing changed.
func TestParkWaitEndsWhenTheAgentStopsIsDeletedOrTakesATask(t *testing.T) {
	cases := []struct {
		name       string
		recheck    func(mock sqlmock.Sqlmock)
		wantStatus int
		wantCode   string
		wantExtra  map[string]interface{}
	}{
		{"no longer online", func(mock sqlmock.Sqlmock) { expectLightRecheck(mock, "provisioning", "open", 0) },
			http.StatusConflict, "LIGHT_AGENT_NOT_AWAKE", map[string]interface{}{"status": "provisioning"}},
		{"deleted", func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(lightLoadSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"name"}))
		}, http.StatusNotFound, "WORKSPACE_NOT_FOUND", nil},
		{"took a task", func(mock sqlmock.Sqlmock) { expectLightRecheck(mock, "online", "open", 1) },
			http.StatusConflict, "LIGHT_AGENT_BUSY", map[string]interface{}{"active_tasks": float64(1)}},
		{"another operation moved it", func(mock sqlmock.Sqlmock) { expectLightRecheck(mock, "online", "", 0) },
			http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeLightReadClock(t)
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			lh, wh, _ := newParkTestHandler(t, api, nil)
			reader := &scriptedGrid{steps: []error{errTestUnreachable}}
			wh.lightScheduleReader = reader.read
			expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
			mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
			tc.recheck(mock)
			rec := recordSQL(t)

			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			assertNoWrites(t, rec())
			body := lightBody(t, w)
			if w.Code != tc.wantStatus || body["code"] != tc.wantCode {
				t.Fatalf("park = %d %v, want %d %s", w.Code, body, tc.wantStatus, tc.wantCode)
			}
			for k, v := range tc.wantExtra {
				if body[k] != v {
					t.Errorf("%s = %v, want %v", k, body[k], v)
				}
			}
			if n := reader.count(); n != 1 || len(api.Calls()) != 0 {
				t.Errorf("reads = %d, CP calls = %v; want one read and no call", n, api.Calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

// A grid that is not a schedule list (or one over 8 MiB) is refused at once,
// before the claim: another try would read the same answer.
func TestParkOfABadGridIsRefusedAtOnceAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{errTestUnreadable, http.StatusUnprocessableEntity, "LIGHT_SCHEDULES_UNREADABLE"},
		{fmt.Errorf("%w: the schedules API answered 404", errLightGridUnreadable), http.StatusUnprocessableEntity, "LIGHT_SCHEDULES_UNREADABLE"},
		{errLightGridTooLarge, http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			fakeLightReadClock(t)
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			lh, wh, _ := newParkTestHandler(t, api, nil)
			reader := &scriptedGrid{steps: []error{tc.err}}
			wh.lightScheduleReader = reader.read
			expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
			mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
			rec := recordSQL(t)

			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			assertNoWrites(t, rec())
			if w.Code != tc.wantStatus || lightBody(t, w)["code"] != tc.wantCode || w.Header().Get("Retry-After") != "" {
				t.Fatalf("park = %d %s (Retry-After %q), want %d %s", w.Code, w.Body.String(), w.Header().Get("Retry-After"), tc.wantStatus, tc.wantCode)
			}
			if n := reader.count(); n != 1 || len(api.Calls()) != 0 {
				t.Errorf("reads = %d, CP calls = %v; want one read and no call", n, api.Calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

// The read after the claim is the grid the park sends. When it fails (the
// agent stopped answering in between, or its grid changed into something
// unreadable), the park is undone: the agent is open again, with last_error.
func TestParkWhoseReadAfterTheClaimFailsGoesBackToOpen(t *testing.T) {
	for _, tc := range []struct {
		name       string
		after      error
		rechecks   int
		wantStatus int
		wantCode   string
	}{
		// After the claim the read has 10 s: tries at 0, 1, 3 and 7 s.
		{"the agent stopped answering", errTestUnreachable, 3, http.StatusConflict, "LIGHT_AGENT_NOT_READY"},
		{"the grid became unreadable", errTestUnreadable, 0, http.StatusUnprocessableEntity, "LIGHT_SCHEDULES_UNREADABLE"},
		{"the grid grew over 8 MiB", errLightGridTooLarge, 0, http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeLightReadClock(t)
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			lh, wh, stops := newParkTestHandler(t, api, nil)
			reader := &scriptedGrid{steps: []error{nil, tc.after}, grid: lightGrid()}
			wh.lightScheduleReader = reader.read
			expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
			mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
			expectLightClaimParking(mock, "owner", 0)
			for i := 0; i < tc.rechecks; i++ {
				expectLightRecheck(mock, "online", "parking", 0)
			}
			expectLightSetState(mock, []string{"parking"}, "open", 1)
			mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, tc.wantCode, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			wh.waitAsyncForTest()
			if w.Code != tc.wantStatus || lightBody(t, w)["code"] != tc.wantCode {
				t.Fatalf("park = %d %s, want %d %s", w.Code, w.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if len(api.Calls()) != 0 || len(*stops) != 0 {
				t.Errorf("CP calls = %v, stops = %v", api.Calls(), *stops)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (back to open with last_error): %v", err)
			}
		})
	}
}

// The monitor's parks (auto-park, a refresh, a continued park) wait only a few
// seconds: a sweep serves other agents, and the refusal it records keeps the
// agent out of the sweeps for the error back-off.
func TestMonitorParkOfAnAgentThatDoesNotAnswerWaitsBriefly(t *testing.T) {
	elapsed := fakeLightReadClock(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	_, wh, _ := newParkTestHandler(t, api, nil)
	reader := &scriptedGrid{steps: []error{errTestUnreachable}}
	wh.lightScheduleReader = reader.read
	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	// 5 s: tries at 0, 1 and 3 s.
	expectLightRecheck(mock, "online", "open", 0)
	expectLightRecheck(mock, "online", "open", 0)

	_, ref := wh.parkLight(context.Background(), nil, lightTestWS, lightTriggerAuto)
	if ref == nil || ref.code() != "LIGHT_AGENT_NOT_READY" {
		t.Fatalf("auto park = %v, want LIGHT_AGENT_NOT_READY", ref)
	}
	if n := reader.count(); n != 3 || elapsed() != 3*time.Second {
		t.Errorf("reads = %d over %s, want 3 over 3s (the monitor's %s)", n, elapsed(), lightMonitorAgentReadBudget)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// readAgentSchedules tells an agent that does not answer (worth another try)
// from one that answers with something that is not a grid (refused): a 5xx,
// 408 or 429, a transport failure and a missing URL are the first; any other
// status, malformed JSON and a body without "schedules" the second.
func TestReadAgentSchedulesTellsAnUnreachableAgentFromABadGrid(t *testing.T) {
	allowLoopbackForTest(t)
	var mu sync.Mutex
	status, body := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st, b := status, body
		mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, b)
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	read := func(url string) error {
		mock := setupTestDB(t)
		_, wh := newLightTestHandler(t, &fakeLightAPI{})
		mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(url))
		if url != "" {
			expectInboundSecret(mock, lightTestWS, "inbound-secret")
		}
		_, err := wh.readAgentSchedules(context.Background(), lightTestWS)
		return err
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		url    string
		want   error
	}{
		{"503 while the runtime starts", http.StatusServiceUnavailable, `{"error":"starting"}`, srv.URL, errLightAgentUnreachable},
		{"502 from something in front of it", http.StatusBadGateway, `<html>bad gateway</html>`, srv.URL, errLightAgentUnreachable},
		{"500", http.StatusInternalServerError, ``, srv.URL, errLightAgentUnreachable},
		{"429", http.StatusTooManyRequests, ``, srv.URL, errLightAgentUnreachable},
		{"408", http.StatusRequestTimeout, ``, srv.URL, errLightAgentUnreachable},
		{"connection refused", http.StatusOK, ``, closedURL, errLightAgentUnreachable},
		{"no registered URL", http.StatusOK, ``, "", errLightAgentUnreachable},
		{"404: no schedules API", http.StatusNotFound, `{"error":"not found"}`, srv.URL, errLightGridUnreadable},
		{"401: refused the secret", http.StatusUnauthorized, `{"error":"unauthorized"}`, srv.URL, errLightGridUnreadable},
		{"malformed JSON", http.StatusOK, `{"schedules":[`, srv.URL, errLightGridUnreadable},
		{"no schedules list", http.StatusOK, `{"items":[]}`, srv.URL, errLightGridUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, body = tc.status, tc.body
			mu.Unlock()
			err := read(tc.url)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			other := errLightGridUnreadable
			if tc.want == errLightGridUnreadable {
				other = errLightAgentUnreachable
			}
			if errors.Is(err, other) {
				t.Errorf("err = %v is both kinds", err)
			}
			if ref := lightAgentReadRefusal(lightTestWS, err); (ref == nil) != (tc.want == errLightAgentUnreachable) {
				t.Errorf("refusal = %v; an unreachable agent is retried, a bad grid refused", ref)
			} else if ref != nil && (ref.Status != http.StatusUnprocessableEntity || ref.code() != "LIGHT_SCHEDULES_UNREADABLE") {
				t.Errorf("refusal = %d %s, want 422 LIGHT_SCHEDULES_UNREADABLE", ref.Status, ref.code())
			}
		})
	}
	if !strings.Contains(fmt.Sprint(lightAgentReadRefusal(lightTestWS, errors.New("workspace lookup: conn reset")).Body), "LIGHT_FAILED") {
		t.Error("an error of core's own is LIGHT_FAILED")
	}
}

// No Light refusal a client acts on is a 502 or a 504: Cloudflare, in front
// of every tenant, replaces such a body with its own page, and the client
// loses the code.
func TestParkReadRefusalsAreNeverAGatewayStatus(t *testing.T) {
	for _, ref := range []*lightRefusal{
		lightAgentNotReadyRefusal("online"),
		lightAgentReadRefusal(lightTestWS, errTestUnreadable),
		lightAgentReadRefusal(lightTestWS, errLightGridTooLarge),
	} {
		if ref.Status == http.StatusBadGateway || ref.Status == http.StatusGatewayTimeout || ref.Status >= 500 && ref.code() != "LIGHT_FAILED" {
			t.Errorf("%s answers %d", ref.code(), ref.Status)
		}
	}
	if r := lightAgentNotReadyRefusal("online"); r.Status != http.StatusConflict || r.RetryAfter == "" {
		t.Errorf("LIGHT_AGENT_NOT_READY = %d Retry-After %q, want 409 with Retry-After", r.Status, r.RetryAfter)
	}
}
