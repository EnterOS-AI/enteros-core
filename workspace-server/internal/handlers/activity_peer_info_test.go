package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Tests for the `?include=peer_info` activity-feed enrichment.
//
// The enrichment is additive + opt-in. When the flag is absent, the
// existing tests (TestActivityList_SourceCanvas, etc.) prove the wire
// shape is unchanged. These tests prove:
//   - When the flag IS set, the LEFT JOIN is issued and the SELECT
//     adds w.name + w.role.
//   - peer_name / peer_role surface from the joined row.
//   - agent_card_url is composed server-side from
//     externalPlatformURL + source_id and appears for non-canvas rows
//     (source_id present).
//   - attachments[] is projected from request_body.params.message.parts
//     for file/image/audio parts.
//   - Canvas rows (source_id NULL) do NOT get peer_name / peer_role /
//     agent_card_url, but DO still appear in the result set (LEFT JOIN
//     preserves them with NULL peer fields).
//   - The `include` query param is comma-separable and only recognizes
//     known flags.

// ---------- includeFlagSet helper unit tests ----------

func TestIncludeFlagSet(t *testing.T) {
	cases := []struct {
		query string
		flag  string
		want  bool
	}{
		{"", "peer_info", false},
		{"peer_info", "peer_info", true},
		{"peer_info,attachments", "peer_info", true},
		{"attachments,peer_info", "peer_info", true},
		{"attachments , peer_info ", "peer_info", true},
		{"peer_infos", "peer_info", false},
		{"peerinfo", "peer_info", false},
		{"peer_info", "", false},
		{",,", "peer_info", false},
	}
	for _, tc := range cases {
		got := includeFlagSet(tc.query, tc.flag)
		if got != tc.want {
			t.Errorf("includeFlagSet(%q, %q) = %v, want %v", tc.query, tc.flag, got, tc.want)
		}
	}
}

// ---------- extractAttachmentsFromRequestBody unit tests ----------

func TestExtractAttachmentsFromRequestBody_Empty(t *testing.T) {
	if got := extractAttachmentsFromRequestBody(nil); got != nil {
		t.Errorf("nil body: want nil, got %v", got)
	}
	if got := extractAttachmentsFromRequestBody([]byte("")); got != nil {
		t.Errorf("empty body: want nil, got %v", got)
	}
	if got := extractAttachmentsFromRequestBody([]byte("not json")); got != nil {
		t.Errorf("non-json body: want nil, got %v", got)
	}
}

func TestExtractAttachmentsFromRequestBody_NoAttachments(t *testing.T) {
	// Text-only message: no file/image/audio parts → nil
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[{"kind":"text","text":"hi"}]}}}`)
	if got := extractAttachmentsFromRequestBody(body); got != nil {
		t.Errorf("text-only: want nil, got %v", got)
	}
}

func TestExtractAttachmentsFromRequestBody_FileKindV1(t *testing.T) {
	// a2a-sdk v1 shape: kind=file, file:{uri,mime_type,name}
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[
		{"kind":"text","text":"see attached"},
		{"kind":"file","file":{"uri":"workspace:foo.pdf","mime_type":"application/pdf","name":"foo.pdf"}}
	]}}}`)
	atts := extractAttachmentsFromRequestBody(body)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if atts[0]["kind"] != "file" {
		t.Errorf("kind: want file, got %v", atts[0]["kind"])
	}
	if atts[0]["uri"] != "workspace:foo.pdf" {
		t.Errorf("uri mismatch: %v", atts[0]["uri"])
	}
	if atts[0]["mime_type"] != "application/pdf" {
		t.Errorf("mime_type mismatch: %v", atts[0]["mime_type"])
	}
	if atts[0]["name"] != "foo.pdf" {
		t.Errorf("name mismatch: %v", atts[0]["name"])
	}
}

func TestExtractAttachmentsFromRequestBody_ImageAndAudio(t *testing.T) {
	// Mixed image + audio parts; both surface
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[
		{"kind":"image","file":{"uri":"workspace:a.png","mime_type":"image/png","name":"a.png"}},
		{"kind":"audio","file":{"uri":"workspace:b.mp3","mime_type":"audio/mpeg","name":"b.mp3"}}
	]}}}`)
	atts := extractAttachmentsFromRequestBody(body)
	if len(atts) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(atts))
	}
	if atts[0]["kind"] != "image" || atts[1]["kind"] != "audio" {
		t.Errorf("kind order: got %v / %v", atts[0]["kind"], atts[1]["kind"])
	}
}

func TestExtractAttachmentsFromRequestBody_LegacyV0TypeDiscriminator(t *testing.T) {
	// Legacy v0 shape: type=file (not kind), inlined fields (no nested .file)
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[
		{"type":"file","uri":"workspace:legacy.txt","mime_type":"text/plain","name":"legacy.txt"}
	]}}}`)
	atts := extractAttachmentsFromRequestBody(body)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if atts[0]["kind"] != "file" || atts[0]["uri"] != "workspace:legacy.txt" || atts[0]["name"] != "legacy.txt" {
		t.Errorf("v0 part not surfaced: %v", atts[0])
	}
}

func TestExtractAttachmentsFromRequestBody_SkipsEmptyParts(t *testing.T) {
	// A "file" part with no uri AND no name is malformed — skip rather
	// than emit a no-info entry.
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[
		{"kind":"file","file":{}},
		{"kind":"file","file":{"name":"only-name.bin"}}
	]}}}`)
	atts := extractAttachmentsFromRequestBody(body)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment (the named one), got %d", len(atts))
	}
	if atts[0]["name"] != "only-name.bin" {
		t.Errorf("expected only-name.bin, got %v", atts[0])
	}
}

func TestExtractAttachmentsFromRequestBody_MalformedShape(t *testing.T) {
	// Various malformed shapes return nil (defensive)
	for _, b := range []string{
		`{}`,
		`{"params":{}}`,
		`{"params":{"message":{}}}`,
		`{"params":{"message":{"parts":"not-a-list"}}}`,
		`{"params":{"message":{"parts":[null,42,"string"]}}}`,
	} {
		if got := extractAttachmentsFromRequestBody([]byte(b)); got != nil {
			t.Errorf("body %q: want nil, got %v", b, got)
		}
	}
}

// ---------- Activity List ?include=peer_info handler tests ----------

func TestActivityList_IncludePeerInfo_IssuesLeftJoin(t *testing.T) {
	// When ?include=peer_info is set, the query must:
	//   1. SELECT include w.name + w.role aliased as peer_name/peer_role
	//   2. FROM contains LEFT JOIN workspaces w ON w.id = activity_logs.source_id
	//   3. WHERE uses qualified activity_logs.workspace_id (disambiguates
	//      from workspaces.id post-JOIN)
	//
	// Pin all three so a future refactor can't silently drop the JOIN or
	// the alias and have the test still pass.
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	peerID := "11111111-2222-3333-4444-555555555555"
	mock.ExpectQuery(
		`SELECT .+w\.name AS peer_name, w\.role AS peer_role FROM activity_logs LEFT JOIN workspaces w ON w\.id = activity_logs\.source_id WHERE activity_logs\.workspace_id = .+`,
	).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
			"peer_name", "peer_role",
		}).
			AddRow("act-1", "ws-1", "a2a_receive", peerID, "ws-1",
				"message/send", "Agent message: hello",
				[]byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[{"kind":"text","text":"hello"}]}}}`),
				nil, nil, nil, "ok", nil, time.Now(),
				"Production Manager", "product manager"))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?include=peer_info", nil)
	c.Request.Host = "platform.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 row, got %d", len(resp))
	}
	r := resp[0]
	if r["peer_name"] != "Production Manager" {
		t.Errorf("peer_name: got %v", r["peer_name"])
	}
	if r["peer_role"] != "product manager" {
		t.Errorf("peer_role: got %v", r["peer_role"])
	}
	wantURL := "https://platform.test/registry/discover/" + peerID
	if r["agent_card_url"] != wantURL {
		t.Errorf("agent_card_url: got %v, want %v", r["agent_card_url"], wantURL)
	}
	// Text-only message has no attachments → omit from envelope
	if _, present := r["attachments"]; present {
		t.Errorf("attachments should be omitted on text-only row; got %v", r["attachments"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_IncludePeerInfo_CanvasRowHasNoPeerFields(t *testing.T) {
	// LEFT JOIN preserves canvas rows (source_id NULL) but their
	// peer_name/peer_role come back as NULL — must omit from the
	// envelope (not emit empty strings or null literals).
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	mock.ExpectQuery(
		`LEFT JOIN workspaces w ON w\.id = activity_logs\.source_id`,
	).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
			"peer_name", "peer_role",
		}).
			// source_id NULL = canvas message; peer columns also NULL.
			AddRow("act-canvas", "ws-1", "a2a_receive", nil, "ws-1",
				"notify", "User said hi",
				[]byte(`{"params":{"message":{"parts":[{"kind":"text","text":"hi"}]}}}`),
				nil, nil, nil, "ok", nil, time.Now(),
				nil, nil))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?include=peer_info", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 row, got %d", len(resp))
	}
	r := resp[0]
	for _, k := range []string{"peer_name", "peer_role", "agent_card_url"} {
		if _, present := r[k]; present {
			t.Errorf("%s should be absent on canvas row; got %v", k, r[k])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_IncludePeerInfo_AttachmentsSurfaceFromRequestBody(t *testing.T) {
	// A peer_agent message with an inline file attachment must have
	// attachments[] populated on the envelope.
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	peerID := "11111111-2222-3333-4444-555555555555"
	mock.ExpectQuery(`LEFT JOIN workspaces`).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
			"peer_name", "peer_role",
		}).
			AddRow("act-with-file", "ws-1", "a2a_receive", peerID, "ws-1",
				"message/send", "Agent message: see attached",
				[]byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"parts":[
					{"kind":"text","text":"see attached"},
					{"kind":"file","file":{"uri":"workspace:foo.pdf","mime_type":"application/pdf","name":"foo.pdf"}}
				]}}}`),
				nil, nil, nil, "ok", nil, time.Now(),
				"Code Reviewer", "code reviewer"))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?include=peer_info", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := resp[0]
	atts, ok := r["attachments"].([]interface{})
	if !ok {
		t.Fatalf("attachments missing or wrong type: %T %v", r["attachments"], r["attachments"])
	}
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d: %v", len(atts), atts)
	}
	att := atts[0].(map[string]interface{})
	if att["kind"] != "file" || att["uri"] != "workspace:foo.pdf" || att["name"] != "foo.pdf" {
		t.Errorf("attachment shape: %v", att)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_IncludePeerInfo_Unset_NoJoinNoExtraFields(t *testing.T) {
	// Back-compat — when ?include=peer_info is NOT passed, the SELECT
	// uses unqualified column refs (no `activity_logs.` prefix) AND no
	// JOIN. Existing tests pass this implicitly; this test pins it
	// explicitly so a future refactor that accidentally turns the JOIN
	// always-on gets caught.
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// Regex pinned: "FROM activity_logs WHERE workspace_id" — no JOIN
	// keyword between FROM and WHERE; no `activity_logs.` qualifier on
	// workspace_id.
	mock.ExpectQuery(`SELECT id, workspace_id,.+ FROM activity_logs WHERE workspace_id = .+`).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}).
			AddRow("act-1", "ws-1", "a2a_receive", "11111111-2222-3333-4444-555555555555", "ws-1",
				"message/send", "Hello",
				nil, nil, nil, nil, "ok", nil, time.Now()))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 row, got %d", len(resp))
	}
	// Confirm no peer_info enrichment leaks into the default envelope.
	for _, k := range []string{"peer_name", "peer_role", "agent_card_url", "attachments"} {
		if _, present := resp[0][k]; present {
			t.Errorf("%s must NOT appear without ?include=peer_info; got %v", k, resp[0][k])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_IncludePeerInfo_UnknownFlagIgnored(t *testing.T) {
	// ?include=bogus must NOT issue the JOIN — only the recognized
	// `peer_info` flag triggers enrichment. The unknown flag is silently
	// ignored (additive, opt-in convention).
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	mock.ExpectQuery(`SELECT id, workspace_id,.+ FROM activity_logs WHERE workspace_id = .+`).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?include=bogus", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Sanity test using the existing test broadcaster setup — verifies the
// extractAttachments helper round-trips through json.Marshal cleanly
// (no map ordering issues, no type-coercion surprises).
func TestExtractAttachmentsFromRequestBody_RoundTripsThroughJSON(t *testing.T) {
	body := []byte(`{"params":{"message":{"parts":[{"kind":"file","file":{"uri":"workspace:r.bin","mime_type":"application/octet-stream","name":"r.bin"}}]}}}`)
	atts := extractAttachmentsFromRequestBody(body)
	b, err := json.Marshal(atts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["uri"] != "workspace:r.bin" {
		t.Fatalf("round-trip mismatch: %v", decoded)
	}
	_ = fmt.Sprintf // keep fmt import live if test trimming removes usage
}
