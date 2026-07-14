package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func runNativeChannelCutoverInventory(t *testing.T, database *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/cutovers/native-channels/inventory", nil)
	NewNativeChannelCutoverInventoryHandler(database).Inventory(c)
	return w
}

func expectNativeChannelInventoryStart(mock sqlmock.Sqlmock, tablePresent bool) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT to_regclass\('public\.workspace_channels'\) IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(tablePresent))
}

func expectNativeChannelInventoryTotals(mock sqlmock.Sqlmock, totalRows, orphanRows int64) {
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\).*FILTER`).
		WillReturnRows(sqlmock.NewRows([]string{"total_rows", "orphan_rows"}).
			AddRow(totalRows, orphanRows))
}

func TestNativeChannelCutoverInventory_ReturnsConsistentZeroInclusiveCounts(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 2, 0)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-empty", int64(0)).
			AddRow("ws-with-rows", int64(2)))
	mock.ExpectCommit()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got nativeChannelCutoverInventoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ContractVersion != 1 || got.TableState != "present" {
		t.Fatalf("contract/table state = %d/%q, want 1/present", got.ContractVersion, got.TableState)
	}
	if got.TotalRows != 2 || got.OrphanRows != 0 || got.WorkspaceRowSum != 2 {
		t.Fatalf("unexpected totals: %+v", got)
	}
	if len(got.Workspaces) != 2 || got.Workspaces[0].WorkspaceID != "ws-empty" || got.Workspaces[0].RowCount != 0 {
		t.Fatalf("zero-inclusive workspace inventory missing: %+v", got.Workspaces)
	}
	for _, forbidden := range []string{"channel_config", "bot_token", "signing_secret", "allowed_users"} {
		if strings.Contains(strings.ToLower(w.Body.String()), forbidden) {
			t.Fatalf("response contains forbidden credential/config field %q: %s", forbidden, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_ReportsRowsOutsideActiveRosterAsOrphans(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 3, 1)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-active", int64(2)))
	mock.ExpectCommit()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got nativeChannelCutoverInventoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TotalRows != 3 || got.OrphanRows != 1 || got.WorkspaceRowSum != 2 {
		t.Fatalf("orphan accounting wrong: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_RealEmptyRosterIsJSONEmptyArray(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 0, 0)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}))
	mock.ExpectCommit()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	workspaces, ok := got["workspaces"].([]any)
	if !ok || len(workspaces) != 0 {
		t.Fatalf("workspaces = %#v, want a real JSON []", got["workspaces"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_TableAbsentIsExplicitConflict(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, false)
	mock.ExpectRollback()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["table_state"] != "absent" {
		t.Fatalf("table_state = %#v, want explicit absent", got["table_state"])
	}
	if _, ok := got["total_rows"]; ok {
		t.Fatalf("absent table must not be normalized into numeric zero: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_QueryErrorFailsClosed(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\).*FILTER`).WillReturnError(errors.New("database unavailable"))
	mock.ExpectRollback()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "database unavailable") {
		t.Fatalf("database error detail leaked: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_ScanErrorDiscardsPartialInventory(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 0, 0)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-valid-prefix", int64(0)).
			AddRow(nil, int64(0)))
	mock.ExpectRollback()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ws-valid-prefix") {
		t.Fatalf("partial inventory leaked after scan failure: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_RowsErrorDiscardsPartialInventory(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 0, 0)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-valid-prefix", int64(0)).
			AddRow("ws-row-error", int64(0)).
			RowError(1, errors.New("iteration failed")))
	mock.ExpectRollback()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ws-valid-prefix") {
		t.Fatalf("partial inventory leaked after rows.Err: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_RejectsDuplicateOrMissingWorkspaceIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{
			name: "duplicate",
			rows: sqlmock.NewRows([]string{"workspace_id", "row_count"}).
				AddRow("ws-1", int64(0)).AddRow("ws-1", int64(0)),
		},
		{
			name: "missing",
			rows: sqlmock.NewRows([]string{"workspace_id", "row_count"}).
				AddRow("", int64(0)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })

			expectNativeChannelInventoryStart(mock, true)
			expectNativeChannelInventoryTotals(mock, 0, 0)
			mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).WillReturnRows(tc.rows)
			mock.ExpectRollback()

			w := runNativeChannelCutoverInventory(t, database)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("sqlmock expectations: %v", err)
			}
		})
	}
}

func TestNativeChannelCutoverInventory_RejectsAccountingMismatch(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	expectNativeChannelInventoryStart(mock, true)
	expectNativeChannelInventoryTotals(mock, 2, 0)
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-1", int64(1)))
	mock.ExpectRollback()

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ws-1") {
		t.Fatalf("mismatched partial inventory leaked: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventory_QueriesNeverSelectCredentialConfig(t *testing.T) {
	for name, query := range map[string]string{
		"totals":           nativeChannelCutoverTotalsQuery,
		"workspace-counts": nativeChannelCutoverWorkspaceCountsQuery,
	} {
		lower := strings.ToLower(query)
		for _, forbidden := range []string{"channel_config", "allowed_users", "bot_token", "signing_secret"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s query selects forbidden config field %q", name, forbidden)
			}
		}
	}
}
