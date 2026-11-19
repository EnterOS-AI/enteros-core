package provisioner

// cp_light.go — the tenant platform's client for the control plane's Enter OS
// Light routes (contract C4, molecule-controlplane internal/handlers/
// workspace_light.go):
//
//	PUT    /cp/workspaces/:id/light                          park: env snapshot + one CronJob per schedule
//	POST   /cp/workspaces/:id/light/resume                   let the schedules fire
//	POST   /cp/workspaces/:id/light/pause                    stop them (the agent is opened)
//	POST   /cp/workspaces/:id/light/schedules/:name/suspend  stop one schedule
//	POST   /cp/workspaces/:id/light/run                      run one schedule now
//	GET    /cp/workspaces/:id/light/jobs                     the runs Kubernetes keeps
//	DELETE /cp/workspaces/:id/light                          remove every Light object
//
// Auth is the provision routes' (provisionAuthHeaders): the shared secret plus
// the tenant's X-Molecule-Admin-Token, which names the org on the CP side.
//
// The PUT body carries the run token and every prompt: it is never logged and
// never quoted in an error. Errors carry the CP's code (LightCPError) so the
// owner-facing API can answer with the same code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// lightResponseLimit bounds a Light response body. The largest legitimate one
// is a jobs listing (a few hundred bytes per Job).
const lightResponseLimit = 8 << 20

// LightSchedule is one schedule as core sends it at park (C4 PUT).
type LightSchedule struct {
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
	// Enabled absent means true on the CP side; core sends false for a
	// schedule the grid disables or core suspended.
	Enabled *bool `json:"enabled,omitempty"`
}

// LightPutRequest is the body of PUT /cp/workspaces/:id/light.
type LightPutRequest struct {
	Schedules       []LightSchedule `json:"schedules"`
	RunToken        string          `json:"run_token"`
	DeadlineSeconds int             `json:"deadline_seconds"`
}

// LightScheduleState is one schedule's CronJob as a Light action left it.
type LightScheduleState struct {
	Name      string `json:"name"`
	CronJob   string `json:"cronjob"`
	Enabled   bool   `json:"enabled"`
	Suspended bool   `json:"suspended"`
}

// LightPutResult answers a park.
type LightPutResult struct {
	Schedules []LightScheduleState `json:"schedules"`
	Removed   []string             `json:"removed"`
	Image     string               `json:"image"`
}

// LightStateResult answers resume and pause. Cancelled names the runs a pause
// deleted before their copy finished: they never ran.
type LightStateResult struct {
	Schedules []LightScheduleState `json:"schedules"`
	Cancelled []string             `json:"cancelled,omitempty"`
}

// LightJob is one run as the CP's jobs listing reports it.
type LightJob struct {
	RunID        string  `json:"run_id"`
	Schedule     string  `json:"schedule"`
	ScheduledFor *string `json:"scheduled_for"`
	CreatedAt    string  `json:"created_at"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	Phase        string  `json:"phase"`
	Reason       string  `json:"reason"`
	ExitCode     *int32  `json:"exit_code"`
}

// LightDeleteResult counts what a Light delete removed.
type LightDeleteResult struct {
	CronJobs int `json:"cronjobs"`
	Jobs     int `json:"jobs"`
	Secrets  int `json:"secrets"`
}

// LightFieldError is one reason the CP refused a Light request (422).
type LightFieldError struct {
	Schedule string `json:"schedule,omitempty"`
	Field    string `json:"field"`
	Reason   string `json:"reason"`
}

// LightCPError is a non-2xx answer from a Light route: the HTTP status and the
// CP's code ({"error":"<CODE>","detail":...} plus key/run_id/invalid).
type LightCPError struct {
	HTTPStatus int
	Code       string
	Detail     string
	Key        string
	RunID      string
	RetryAfter string
	Invalid    []LightFieldError
}

func (e *LightCPError) Error() string {
	msg := fmt.Sprintf("cp light: %d %s", e.HTTPStatus, e.Code)
	if e.Key != "" {
		msg += " (key " + e.Key + ")"
	}
	if e.RunID != "" {
		msg += " (run " + e.RunID + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Unwrap makes a 503 (the CP's own LIGHT_BACKEND_UNAVAILABLE, or any 502-504
// without a code) match ErrLightCPUnavailable: retryable, nothing changed.
func (e *LightCPError) Unwrap() error {
	if e.HTTPStatus == http.StatusServiceUnavailable || e.HTTPStatus == http.StatusBadGateway || e.HTTPStatus == http.StatusGatewayTimeout {
		return ErrLightCPUnavailable
	}
	return nil
}

// ErrLightCPUnavailable: the control plane could not be reached or answered
// that its substrate did not respond. Retryable.
var ErrLightCPUnavailable = errors.New("cp light: control plane unavailable")

// ErrLightNotConfigured: this tenant platform has no control plane (self-host),
// so it cannot run Light schedules.
var ErrLightNotConfigured = errors.New("cp light: no control plane configured")

// LightCPCode returns the CP's error code of err, or "".
func LightCPCode(err error) string {
	var ce *LightCPError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// LightAPI is the C4 surface core calls. *CPProvisioner implements it; handler
// tests substitute a fake.
type LightAPI interface {
	PutLight(ctx context.Context, workspaceID string, req LightPutRequest) (LightPutResult, error)
	ResumeLight(ctx context.Context, workspaceID string) (LightStateResult, error)
	PauseLight(ctx context.Context, workspaceID string) (LightStateResult, error)
	SuspendLightSchedule(ctx context.Context, workspaceID, schedule string) (LightScheduleState, error)
	RunLightNow(ctx context.Context, workspaceID, schedule string) (string, error)
	ListLightJobs(ctx context.Context, workspaceID string) ([]LightJob, error)
	DeleteLight(ctx context.Context, workspaceID string) (LightDeleteResult, error)
}

var _ LightAPI = (*CPProvisioner)(nil)

func lightPath(workspaceID string, rest string) string {
	return "/cp/workspaces/" + url.PathEscape(workspaceID) + "/light" + rest
}

// PutLight snapshots the agent's env and writes one suspended CronJob per
// schedule (C4 PUT). Needs the agent awake.
func (p *CPProvisioner) PutLight(ctx context.Context, workspaceID string, req LightPutRequest) (LightPutResult, error) {
	if req.Schedules == nil {
		req.Schedules = []LightSchedule{}
	}
	var out LightPutResult
	err := p.lightDo(ctx, http.MethodPut, lightPath(workspaceID, ""), req, &out)
	return out, err
}

// ResumeLight lets the parked agent's schedules fire (C4 resume).
func (p *CPProvisioner) ResumeLight(ctx context.Context, workspaceID string) (LightStateResult, error) {
	var out LightStateResult
	err := p.lightDo(ctx, http.MethodPost, lightPath(workspaceID, "/resume"), nil, &out)
	return out, err
}

// PauseLight suspends every schedule of the agent (C4 pause).
func (p *CPProvisioner) PauseLight(ctx context.Context, workspaceID string) (LightStateResult, error) {
	var out LightStateResult
	err := p.lightDo(ctx, http.MethodPost, lightPath(workspaceID, "/pause"), nil, &out)
	return out, err
}

// SuspendLightSchedule suspends one schedule (C4 schedules/{name}/suspend).
func (p *CPProvisioner) SuspendLightSchedule(ctx context.Context, workspaceID, schedule string) (LightScheduleState, error) {
	var out struct {
		Schedule LightScheduleState `json:"schedule"`
	}
	err := p.lightDo(ctx, http.MethodPost, lightPath(workspaceID, "/schedules/"+url.PathEscape(schedule)+"/suspend"), nil, &out)
	return out.Schedule, err
}

// RunLightNow starts one schedule's run (C4 run now) and returns its run id.
func (p *CPProvisioner) RunLightNow(ctx context.Context, workspaceID, schedule string) (string, error) {
	var out struct {
		RunID string `json:"run_id"`
	}
	err := p.lightDo(ctx, http.MethodPost, lightPath(workspaceID, "/run"), map[string]string{"schedule": schedule}, &out)
	return out.RunID, err
}

// ListLightJobs lists the agent's runs (C4 jobs). Never nil on success.
func (p *CPProvisioner) ListLightJobs(ctx context.Context, workspaceID string) ([]LightJob, error) {
	var out []LightJob
	if err := p.lightDo(ctx, http.MethodGet, lightPath(workspaceID, "/jobs"), nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []LightJob{}
	}
	return out, nil
}

// DeleteLight removes every Light object of the agent (C4 DELETE, idempotent).
func (p *CPProvisioner) DeleteLight(ctx context.Context, workspaceID string) (LightDeleteResult, error) {
	var out struct {
		Deleted LightDeleteResult `json:"deleted"`
	}
	err := p.lightDo(ctx, http.MethodDelete, lightPath(workspaceID, ""), nil, &out)
	return out.Deleted, err
}

// lightDo performs one Light call. Any 2xx is success (the CP answers 200, and
// 201 for run now). A non-2xx becomes *LightCPError with the CP's code; a
// transport failure wraps ErrLightCPUnavailable. Neither the request body nor
// the raw response is ever placed in the error.
func (p *CPProvisioner) lightDo(ctx context.Context, method, path string, in, out any) error {
	if p == nil || p.httpClient == nil || p.baseURL == "" {
		return ErrLightNotConfigured
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("cp light: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.baseURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("cp light: build %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	p.provisionAuthHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		// The transport error names the URL, never the body.
		return fmt.Errorf("%w: %s %s: %v", ErrLightCPUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, lightResponseLimit+1))
	if err != nil {
		return fmt.Errorf("%w: read %s %s: %v", ErrLightCPUnavailable, method, path, err)
	}
	if len(raw) > lightResponseLimit {
		return fmt.Errorf("cp light: %s %s: response larger than %d bytes", method, path, lightResponseLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return lightErrorFromResponse(resp, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cp light: decode %s %s: %w", method, path, err)
	}
	return nil
}

// lightErrorFromResponse reads the CP's Light error body. A body that is not
// the documented shape still yields the status; its bytes are not kept.
func lightErrorFromResponse(resp *http.Response, raw []byte) *LightCPError {
	ce := &LightCPError{HTTPStatus: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	var body struct {
		Error   string            `json:"error"`
		Detail  string            `json:"detail"`
		Key     string            `json:"key"`
		RunID   string            `json:"run_id"`
		Invalid []LightFieldError `json:"invalid"`
	}
	if json.Unmarshal(raw, &body) == nil {
		ce.Code, ce.Detail, ce.Key, ce.RunID, ce.Invalid = body.Error, body.Detail, body.Key, body.RunID, body.Invalid
	}
	if ce.Code == "" {
		ce.Code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
	}
	return ce
}
