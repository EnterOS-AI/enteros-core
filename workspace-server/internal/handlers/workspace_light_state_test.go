package handlers

// workspace_light_state_test.go — every workspace object of GET /workspaces
// and GET /workspaces/:id carries light_state (Enter OS Light): the column's
// value, or null for an ordinary agent. The canvas hydrates its nodes' Light
// badge and Light-aware tabs from it.

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// workspaceSelectEndsWithLightState pins the SELECT itself. sqlmock returns the
// scripted columns whatever the query says, so only the query text proves the
// database is asked for light_state, last, where scanWorkspaceRow reads it.
const workspaceSelectEndsWithLightState = `COALESCE\(w\.mcp_surface::text, ''\),\s+COALESCE\(w\.light_state, ''\)\s+FROM workspaces w`

// lightStateRow is a wsColumns row whose light_state column holds lightState.
func lightStateRow(id, lightState string) []driver.Value {
	row := workspaceListTestRow(id, int64(1))
	row[len(row)-1] = lightState
	return row
}

func TestWorkspaceListCarriesLightState(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	mock.ExpectQuery(`SELECT w\.id, w\.name[\s\S]+` + workspaceSelectEndsWithLightState + `[\s\S]+WHERE w\.status != 'removed'`).
		WillReturnRows(sqlmock.NewRows(wsColumns).
			AddRow(lightStateRow("ws-parked", "parked")...).
			AddRow(lightStateRow("ws-plain", "")...))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("list body is not a JSON array: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list returned %d workspaces, want 2", len(got))
	}
	if v, ok := got[0]["light_state"]; !ok || v != "parked" {
		t.Errorf("Light agent: light_state = %v (present %v), want \"parked\"", v, ok)
	}
	if v, ok := got[1]["light_state"]; !ok || v != nil {
		t.Errorf("ordinary agent: light_state = %v (present %v), want an explicit null", v, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceGetCarriesLightState(t *testing.T) {
	cases := []struct {
		name   string
		column string
		want   interface{}
	}{
		{"light agent", "opening", "opening"},
		{"ordinary agent", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			id := "cccccccc-5272-0000-0000-000000000000"
			mock.ExpectQuery(`SELECT w\.id, w\.name[\s\S]+` + workspaceSelectEndsWithLightState + `[\s\S]+WHERE w\.id = \$1`).
				WithArgs(id).
				WillReturnRows(sqlmock.NewRows(wsColumns).AddRow(lightStateRow(id, tc.column)...))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: id}}
			c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+id, nil)
			handler.Get(c)

			if w.Code != http.StatusOK {
				t.Fatalf("get = %d %s", w.Code, w.Body.String())
			}
			var got map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("get body is not a JSON object: %v", err)
			}
			if v, ok := got["light_state"]; !ok || v != tc.want {
				t.Errorf("light_state = %v (present %v), want %v", v, ok, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}
