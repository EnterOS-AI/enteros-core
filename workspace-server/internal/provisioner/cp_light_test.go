package provisioner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const lightTestWS = "be7540cf-d160-4e2a-9b1c-0123456789ab"

// lightCPCall is one request the fake control plane saw.
type lightCPCall struct {
	method, path, rawPath, body, bearer, adminToken, contentType string
}

// fakeLightCP serves one scripted answer and records every request.
func fakeLightCP(t *testing.T, status int, body string, headers map[string]string) (*CPProvisioner, *[]lightCPCall) {
	t.Helper()
	var calls []lightCPCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls = append(calls, lightCPCall{
			method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(), body: string(raw),
			bearer: r.Header.Get("Authorization"), adminToken: r.Header.Get("X-Molecule-Admin-Token"),
			contentType: r.Header.Get("Content-Type"),
		})
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &CPProvisioner{baseURL: srv.URL, orgID: "org-1", sharedSecret: "s3cret", adminToken: "adm", httpClient: srv.Client()}, &calls
}

func lightBoolPtr(b bool) *bool { return &b }

func TestLightCPPutSendsTheContractBody(t *testing.T) {
	p, calls := fakeLightCP(t, http.StatusOK, `{"ok":true,"schedules":[],"removed":[],"image":"img"}`, nil)
	_, err := p.PutLight(context.Background(), lightTestWS, LightPutRequest{
		Schedules: []LightSchedule{
			{Name: "daily", Cron: "0 9 * * *", Timezone: "", Prompt: "p", Enabled: lightBoolPtr(false)},
			{Name: "weekly", Cron: "0 9 * * 1", Timezone: "UTC", Prompt: "q"},
		},
		RunToken:        "run-tok",
		DeadlineSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("PutLight: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.method != http.MethodPut || c.path != "/cp/workspaces/"+lightTestWS+"/light" {
		t.Errorf("request = %s %s", c.method, c.path)
	}
	if c.bearer != "Bearer s3cret" || c.adminToken != "adm" || c.contentType != "application/json" {
		t.Errorf("headers: bearer=%q admin=%q content-type=%q", c.bearer, c.adminToken, c.contentType)
	}
	want := `{"schedules":[{"name":"daily","cron":"0 9 * * *","timezone":"","prompt":"p","enabled":false},{"name":"weekly","cron":"0 9 * * 1","timezone":"UTC","prompt":"q"}],"run_token":"run-tok","deadline_seconds":3600}`
	if c.body != want {
		t.Errorf("body =\n%s\nwant\n%s", c.body, want)
	}
}

func TestLightCPPutWithNoSchedulesSendsAnEmptyList(t *testing.T) {
	p, calls := fakeLightCP(t, http.StatusOK, `{"ok":true,"schedules":[],"removed":["a"],"image":"img"}`, nil)
	res, err := p.PutLight(context.Background(), lightTestWS, LightPutRequest{RunToken: "t", DeadlineSeconds: 3600})
	if err != nil {
		t.Fatalf("PutLight: %v", err)
	}
	if !strings.Contains((*calls)[0].body, `"schedules":[]`) {
		t.Errorf("body %s: an empty set must be [] (the CP removes every CronJob), not null", (*calls)[0].body)
	}
	if !reflect.DeepEqual(res.Removed, []string{"a"}) || res.Image != "img" {
		t.Errorf("result = %+v", res)
	}
}

func TestLightCPRoutesAndMethods(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name            string
		call            func(p *CPProvisioner) error
		method, rawPath string
		body            string
		responseStatus  int
		responseBody    string
	}{
		{"resume", func(p *CPProvisioner) error { _, err := p.ResumeLight(ctx, lightTestWS); return err },
			http.MethodPost, "/cp/workspaces/" + lightTestWS + "/light/resume", "", 200, `{"ok":true,"schedules":[]}`},
		{"pause", func(p *CPProvisioner) error { _, err := p.PauseLight(ctx, lightTestWS); return err },
			http.MethodPost, "/cp/workspaces/" + lightTestWS + "/light/pause", "", 200, `{"ok":true,"schedules":[]}`},
		{"suspend", func(p *CPProvisioner) error {
			_, err := p.SuspendLightSchedule(ctx, lightTestWS, "日报 a")
			return err
		},
			http.MethodPost, "/cp/workspaces/" + lightTestWS + "/light/schedules/%E6%97%A5%E6%8A%A5%20a/suspend", "", 200, `{"ok":true,"schedule":{"name":"日报 a","cronjob":"c","enabled":true,"suspended":true}}`},
		{"run", func(p *CPProvisioner) error { _, err := p.RunLightNow(ctx, lightTestWS, "daily"); return err },
			http.MethodPost, "/cp/workspaces/" + lightTestWS + "/light/run", `{"schedule":"daily"}`, 201, `{"run_id":"light-x"}`},
		{"jobs", func(p *CPProvisioner) error { _, err := p.ListLightJobs(ctx, lightTestWS); return err },
			http.MethodGet, "/cp/workspaces/" + lightTestWS + "/light/jobs", "", 200, `[]`},
		{"delete", func(p *CPProvisioner) error { _, err := p.DeleteLight(ctx, lightTestWS); return err },
			http.MethodDelete, "/cp/workspaces/" + lightTestWS + "/light", "", 200, `{"ok":true,"deleted":{"cronjobs":0,"jobs":0,"secrets":0}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, calls := fakeLightCP(t, tc.responseStatus, tc.responseBody, nil)
			if err := tc.call(p); err != nil {
				t.Fatalf("call: %v", err)
			}
			c := (*calls)[0]
			if c.method != tc.method || c.rawPath != tc.rawPath || c.body != tc.body {
				t.Errorf("request = %s %s body %q; want %s %s body %q", c.method, c.rawPath, c.body, tc.method, tc.rawPath, tc.body)
			}
			if c.bearer != "Bearer s3cret" || c.adminToken != "adm" {
				t.Errorf("auth headers missing: bearer=%q admin=%q", c.bearer, c.adminToken)
			}
		})
	}
}

func TestLightCPDecodesEverySuccessShape(t *testing.T) {
	ctx := context.Background()

	p, _ := fakeLightCP(t, 200, `{"ok":true,"schedules":[{"name":"a","cronjob":"light-1-2","enabled":true,"suspended":true}],"removed":["b"],"image":"reg/img@sha256:1"}`, nil)
	put, err := p.PutLight(ctx, lightTestWS, LightPutRequest{RunToken: "t"})
	if err != nil || !reflect.DeepEqual(put, LightPutResult{
		Schedules: []LightScheduleState{{Name: "a", CronJob: "light-1-2", Enabled: true, Suspended: true}},
		Removed:   []string{"b"}, Image: "reg/img@sha256:1",
	}) {
		t.Errorf("put = %+v, %v", put, err)
	}

	p, _ = fakeLightCP(t, 200, `{"ok":true,"schedules":[],"cancelled":["r1","r2"]}`, nil)
	paused, err := p.PauseLight(ctx, lightTestWS)
	if err != nil || !reflect.DeepEqual(paused.Cancelled, []string{"r1", "r2"}) {
		t.Errorf("pause = %+v, %v", paused, err)
	}

	p, _ = fakeLightCP(t, 201, `{"run_id":"light-be7540cfd160-1a2b3c4d-m1763000000"}`, nil)
	runID, err := p.RunLightNow(ctx, lightTestWS, "daily")
	if err != nil || runID != "light-be7540cfd160-1a2b3c4d-m1763000000" {
		t.Errorf("run = %q, %v", runID, err)
	}

	p, _ = fakeLightCP(t, 200, `[]`, nil)
	jobs, err := p.ListLightJobs(ctx, lightTestWS)
	if err != nil || jobs == nil || len(jobs) != 0 {
		t.Errorf("jobs = %#v, %v; want a non-nil empty list", jobs, err)
	}

	p, _ = fakeLightCP(t, 200, `[{"run_id":"r1","schedule":"daily","scheduled_for":"2026-11-13T01:00:00Z","created_at":"2026-11-13T00:59:00Z","started_at":null,"finished_at":"2026-11-13T02:00:00Z","phase":"failed","reason":"OOMKilled","exit_code":137}]`, nil)
	jobs, err = p.ListLightJobs(ctx, lightTestWS)
	if err != nil || len(jobs) != 1 || jobs[0].Reason != "OOMKilled" || jobs[0].ExitCode == nil || *jobs[0].ExitCode != 137 ||
		jobs[0].StartedAt != nil || jobs[0].ScheduledFor == nil || *jobs[0].ScheduledFor != "2026-11-13T01:00:00Z" {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}

	p, _ = fakeLightCP(t, 200, `{"ok":true,"deleted":{"cronjobs":2,"jobs":1,"secrets":1}}`, nil)
	del, err := p.DeleteLight(ctx, lightTestWS)
	if err != nil || del != (LightDeleteResult{CronJobs: 2, Jobs: 1, Secrets: 1}) {
		t.Errorf("delete = %+v, %v", del, err)
	}
}

func TestLightCPErrorsCarryTheContractCode(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		status      int
		body        string
		headers     map[string]string
		want        LightCPError
		unavailable bool
	}{
		{"run active", 409, `{"error":"LIGHT_RUN_ACTIVE","detail":"d","run_id":"r1"}`, nil,
			LightCPError{HTTPStatus: 409, Code: "LIGHT_RUN_ACTIVE", Detail: "d", RunID: "r1"}, false},
		{"env unsupported", 409, `{"error":"LIGHT_ENV_UNSUPPORTED","detail":"d","key":"POD_IP"}`, nil,
			LightCPError{HTTPStatus: 409, Code: "LIGHT_ENV_UNSUPPORTED", Detail: "d", Key: "POD_IP"}, false},
		{"invalid", 422, `{"error":"LIGHT_INVALID","detail":"d","invalid":[{"schedule":"a","field":"cron","reason":"r"}]}`, nil,
			LightCPError{HTTPStatus: 422, Code: "LIGHT_INVALID", Detail: "d", Invalid: []LightFieldError{{Schedule: "a", Field: "cron", Reason: "r"}}}, false},
		{"too large", 413, `{"error":"LIGHT_TOO_LARGE","detail":"d"}`, nil,
			LightCPError{HTTPStatus: 413, Code: "LIGHT_TOO_LARGE", Detail: "d"}, false},
		{"unsupported", 501, `{"error":"LIGHT_UNSUPPORTED","detail":"d"}`, nil,
			LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED", Detail: "d"}, false},
		{"schedule not found", 404, `{"error":"LIGHT_SCHEDULE_NOT_FOUND","detail":"d"}`, nil,
			LightCPError{HTTPStatus: 404, Code: "LIGHT_SCHEDULE_NOT_FOUND", Detail: "d"}, false},
		{"backend unavailable", 503, `{"error":"LIGHT_BACKEND_UNAVAILABLE","detail":"d"}`, map[string]string{"Retry-After": "10"},
			LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE", Detail: "d", RetryAfter: "10"}, true},
		{"gateway html", 502, `<html>bad gateway</html>`, nil,
			LightCPError{HTTPStatus: 502, Code: "HTTP_502"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := fakeLightCP(t, tc.status, tc.body, tc.headers)
			_, err := p.RunLightNow(ctx, lightTestWS, "daily")
			var ce *LightCPError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *LightCPError", err)
			}
			if !reflect.DeepEqual(*ce, tc.want) {
				t.Errorf("error = %+v, want %+v", *ce, tc.want)
			}
			if got := errors.Is(err, ErrLightCPUnavailable); got != tc.unavailable {
				t.Errorf("errors.Is(err, ErrLightCPUnavailable) = %v, want %v", got, tc.unavailable)
			}
			if LightCPCode(err) != tc.want.Code {
				t.Errorf("LightCPCode = %q", LightCPCode(err))
			}
		})
	}
}

func TestLightCPTransportErrorIsUnavailable(t *testing.T) {
	p := &CPProvisioner{baseURL: "http://127.0.0.1:1", sharedSecret: "s", adminToken: "a", httpClient: &http.Client{Timeout: 500 * time.Millisecond}}
	_, err := p.ResumeLight(context.Background(), lightTestWS)
	if !errors.Is(err, ErrLightCPUnavailable) {
		t.Fatalf("err = %v, want ErrLightCPUnavailable", err)
	}
}

func TestLightCPWithoutAControlPlaneIsNotConfigured(t *testing.T) {
	var p *CPProvisioner
	if _, err := p.ListLightJobs(context.Background(), lightTestWS); !errors.Is(err, ErrLightNotConfigured) {
		t.Fatalf("nil provisioner: err = %v, want ErrLightNotConfigured", err)
	}
}

func TestLightCPNeverEchoesTheRunToken(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	p, _ := fakeLightCP(t, 500, `{"error":"LIGHT_FAILED","detail":"the Light operation failed; the control plane logged the cause"}`, nil)
	_, err := p.PutLight(context.Background(), lightTestWS, LightPutRequest{RunToken: "run-tok-SECRET-1", Schedules: []LightSchedule{{Name: "a", Cron: "0 9 * * *", Prompt: "prompt-SECRET"}}})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, secret := range []string{"run-tok-SECRET-1", "prompt-SECRET"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Errorf("%q leaked into the error or the log: err=%q log=%q", secret, err.Error(), logs.String())
		}
	}
}

func TestLightCPBoundsTheResponseBody(t *testing.T) {
	p, _ := fakeLightCP(t, 200, "["+strings.Repeat(" ", lightResponseLimit+10)+"]", nil)
	if _, err := p.ListLightJobs(context.Background(), lightTestWS); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want a size refusal", err)
	}
}

// TestBuildCPTenantEnvCarriesMoleculeLight: the env core sends the control
// plane keeps MOLECULE_LIGHT=1 (C1), which the CP's park requires in the live
// Deployment.
func TestBuildCPTenantEnvCarriesMoleculeLight(t *testing.T) {
	env := buildCPTenantEnv(WorkspaceConfig{
		EnvVars:             map[string]string{"MOLECULE_LIGHT": "1", "MOLECULE_CONTINUOUS": "false"},
		WorkspaceSecretKeys: map[string]struct{}{"MOLECULE_LIGHT": {}, "MOLECULE_CONTINUOUS": {}},
	})
	if env["MOLECULE_LIGHT"] != "1" || env["MOLECULE_CONTINUOUS"] != "false" {
		t.Fatalf("CP env = %v", env)
	}
}
