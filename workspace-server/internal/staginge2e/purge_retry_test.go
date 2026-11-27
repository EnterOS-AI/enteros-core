package staginge2e

import (
	"net/http"
	"testing"
)

func TestIsActivePurgeInProgress(t *testing.T) {
	tests := []struct {
		name   string
		status int
		detail string
		want   bool
	}{
		{
			name:   "500 another purge is active",
			status: http.StatusInternalServerError,
			detail: `purge: another purge is active for this organization`,
			want:   true,
		},
		{
			name:   "409 case-insensitive another purge",
			status: http.StatusConflict,
			detail: `Another Purge Is Active`,
			want:   true,
		},
		{
			name:   "500 unrelated error",
			status: http.StatusInternalServerError,
			detail: `database connection refused`,
			want:   false,
		},
		{
			name:   "400 does not count",
			status: http.StatusBadRequest,
			detail: `another purge is active`,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActivePurgeInProgress(tc.status, tc.detail); got != tc.want {
				t.Fatalf("isActivePurgeInProgress(%d, %q) = %v, want %v", tc.status, tc.detail, got, tc.want)
			}
		})
	}
}

func TestWorkspaceIdleFromBody(t *testing.T) {
	tests := []struct {
		name      string
		httpCode  int
		body      string
		wantIdle  bool
		wantState string
		wantTasks int
	}{
		{
			name:      "online and zero active tasks",
			httpCode:  http.StatusOK,
			body:      `{"status":"online","active_tasks":0,"url":"https://ws.example"}`,
			wantIdle:  true,
			wantState: "online",
			wantTasks: 0,
		},
		{
			name:      "online with active tasks is not idle",
			httpCode:  http.StatusOK,
			body:      `{"status":"online","active_tasks":1}`,
			wantIdle:  false,
			wantState: "online",
			wantTasks: 1,
		},
		{
			name:      "hibernated with zero tasks is not idle",
			httpCode:  http.StatusOK,
			body:      `{"status":"hibernated","active_tasks":0}`,
			wantIdle:  false,
			wantState: "hibernated",
			wantTasks: 0,
		},
		{
			name:      "non-200 fails closed",
			httpCode:  http.StatusBadGateway,
			body:      `{"status":"online","active_tasks":0}`,
			wantIdle:  false,
			wantState: "",
			wantTasks: 0,
		},
		{
			name:      "malformed active_tasks fails closed",
			httpCode:  http.StatusOK,
			body:      `{"status":"online","active_tasks":"1"}`,
			wantIdle:  false,
			wantState: "online",
			wantTasks: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idle, st, tasks := workspaceIdleFromBody(tc.httpCode, tc.body)
			if idle != tc.wantIdle || st != tc.wantState || tasks != tc.wantTasks {
				t.Fatalf("got (%v, %q, %d), want (%v, %q, %d)",
					idle, st, tasks, tc.wantIdle, tc.wantState, tc.wantTasks)
			}
		})
	}
}
