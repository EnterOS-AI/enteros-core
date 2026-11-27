package staginge2e

import (
	"encoding/json"
	"net/http"
	"strings"
)

// isActivePurgeInProgress reports whether the control-plane purge response
// indicates another teardown purge is already in flight for the org (core#5346).
func isActivePurgeInProgress(status int, detail string) bool {
	if status != http.StatusInternalServerError && status != http.StatusConflict {
		return false
	}
	return strings.Contains(strings.ToLower(detail), "another purge is active")
}

// workspaceIdleFromBody parses a GET /workspaces/:id payload and reports whether
// the workspace is online with zero active tasks.
func workspaceIdleFromBody(httpStatus int, body string) (idle bool, status string, activeTasks int) {
	if httpStatus != http.StatusOK {
		return false, "", 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false, "", 0
	}
	if rawStatus, ok := m["status"]; ok {
		_ = json.Unmarshal(rawStatus, &status)
	}
	if rawTasks, ok := m["active_tasks"]; ok {
		if err := json.Unmarshal(rawTasks, &activeTasks); err != nil {
			return false, status, -1
		}
	}
	return status == "online" && activeTasks == 0, status, activeTasks
}
