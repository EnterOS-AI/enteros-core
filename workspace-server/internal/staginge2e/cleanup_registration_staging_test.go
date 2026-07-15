//go:build staging_e2e

package staginge2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceLifecycleRegisterTenantCleanupDeletesExactSlug(t *testing.T) {
	const (
		slug  = "e2e-cleanup-contract"
		token = "test-admin-token"
	)

	type capturedRequest struct {
		method      string
		path        string
		authorize   string
		contentType string
		body        string
	}
	requests := make(chan capturedRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			authorize:   r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(body),
		}
		if r.Method == http.MethodGet && r.URL.Path == "/cp/admin/tenants/"+slug+"/boot-events" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"org not found","slug":"`+slug+`"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := stagingCfg{cpBase: server.URL, adminToken: token}
	if ok := t.Run("registered cleanup", func(t *testing.T) {
		registerTenantCleanup(t, cfg, slug)
	}); !ok {
		t.Fatal("registered cleanup subtest failed")
	}

	var got capturedRequest
	select {
	case got = <-requests:
	default:
		t.Fatal("registered cleanup did not issue a DELETE request")
	}
	if got.method != http.MethodDelete {
		t.Fatalf("cleanup method = %q, want DELETE", got.method)
	}
	if got.path != "/cp/admin/tenants/"+slug {
		t.Fatalf("cleanup path = %q, want exact slug path", got.path)
	}
	if got.authorize != "Bearer "+token {
		t.Fatalf("cleanup authorization header = %q, want test bearer", got.authorize)
	}
	if got.contentType != "application/json" {
		t.Fatalf("cleanup content type = %q, want application/json", got.contentType)
	}
	var confirmation struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(got.body), &confirmation); err != nil {
		t.Fatalf("decode cleanup confirmation: %v", err)
	}
	if confirmation.Confirm != slug {
		t.Fatalf("cleanup confirmation = %q, want exact slug", confirmation.Confirm)
	}
	select {
	case verify := <-requests:
		if verify.method != http.MethodGet || verify.path != "/cp/admin/tenants/"+slug+"/boot-events" {
			t.Fatalf("cleanup verification request = %+v, want exact tenant identity GET", verify)
		}
	default:
		t.Fatal("registered cleanup did not verify exact org absence")
	}
	select {
	case extra := <-requests:
		t.Fatalf("cleanup issued an unexpected third request: %+v", extra)
	default:
	}
}

func TestDeleteTenantAndVerifyRetriesLifecycleConflictAndConfirmsAbsence(t *testing.T) {
	const (
		slug  = "e2e-cleanup-retry-contract"
		token = "test-admin-token"
	)

	var deleteCalls, identityCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("authorization header = %q, want test bearer", got)
		}
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/cp/admin/tenants/"+slug:
			deleteCalls++
			w.Header().Set("Content-Type", "application/json")
			if deleteCalls == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"organization has an active lifecycle operation"}`)
				return
			}
			_, _ = io.WriteString(w, `{"deleted":true,"slug":"`+slug+`"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/cp/admin/tenants/"+slug+"/boot-events":
			identityCalls++
			if got := r.URL.Query().Get("limit"); got != "1" {
				t.Errorf("identity limit = %q, want 1", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"org not found","slug":"`+slug+`"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	err := deleteTenantAndVerify(
		stagingCfg{cpBase: server.URL, adminToken: token},
		slug,
		250*time.Millisecond,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("deleteTenantAndVerify: %v", err)
	}
	if deleteCalls != 2 {
		t.Fatalf("DELETE calls = %d, want conflict retry plus success", deleteCalls)
	}
	if identityCalls != 1 {
		t.Fatalf("identity calls = %d, want exact absence verification", identityCalls)
	}
}

func TestDeleteTenantAndVerifyFailsClosedWhenConflictNeverClears(t *testing.T) {
	const slug = "e2e-cleanup-conflict-contract"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"organization has an active lifecycle operation"}`)
	}))
	defer server.Close()

	err := deleteTenantAndVerify(
		stagingCfg{cpBase: server.URL, adminToken: "test-admin-token"},
		slug,
		5*time.Millisecond,
		time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("persistent conflict error = %v, want bounded HTTP 409 failure", err)
	}
}

func TestDeleteTenantAndVerifyRefusesUntrustedControlPlaneHost(t *testing.T) {
	err := deleteTenantAndVerify(
		stagingCfg{cpBase: "https://example.invalid", adminToken: "must-not-be-sent"},
		"e2e-cleanup-host-contract",
		time.Second,
		time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "refusing to send staging admin bearer") {
		t.Fatalf("untrusted-host error = %v, want bearer exfiltration guard", err)
	}

	err = deleteTenantAndVerify(
		stagingCfg{cpBase: "http://127.0.0.1:1", adminToken: "must-not-be-sent"},
		"e2e-cleanup/../../unsafe",
		time.Second,
		time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "refusing cleanup for non-E2E slug") {
		t.Fatalf("unsafe-slug error = %v, want exact-slug path guard", err)
	}
}

func TestExactTenantAbsentUsesIdentityBoundEndpoint(t *testing.T) {
	const slug = "e2e-cleanup-identity-contract"
	tests := []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
		wantErr    bool
	}{
		{name: "404 is authoritative absence", status: http.StatusNotFound, body: `{"error":"org not found"}`, wantAbsent: true},
		{name: "matching 200 is present", status: http.StatusOK, body: `{"slug":"` + slug + `","events":[]}`},
		{name: "mismatched 200 fails closed", status: http.StatusOK, body: `{"slug":"e2e-other"}`, wantErr: true},
		{name: "malformed 200 fails closed", status: http.StatusOK, body: `{`, wantErr: true},
		{name: "server error is inconclusive", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/cp/admin/tenants/"+slug+"/boot-events" || r.URL.Query().Get("limit") != "1" {
					t.Errorf("identity request = %s %s, want exact tenant boot-events?limit=1", r.Method, r.URL.String())
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			absent, err := exactTenantAbsent(
				&http.Client{Timeout: time.Second},
				stagingCfg{cpBase: server.URL, adminToken: "test-admin-token"},
				slug,
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}
			if absent != tc.wantAbsent {
				t.Fatalf("absent = %v, want %v", absent, tc.wantAbsent)
			}
		})
	}
}

func TestDeleteTenantAndVerifyAcceptsAlreadyAbsentTenant(t *testing.T) {
	const slug = "e2e-cleanup-already-absent"
	var deleteCalls, identityCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNotFound)
		case http.MethodGet:
			identityCalls++
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	if err := deleteTenantAndVerify(
		stagingCfg{cpBase: server.URL, adminToken: "test-admin-token"},
		slug,
		250*time.Millisecond,
		time.Millisecond,
	); err != nil {
		t.Fatalf("already absent cleanup: %v", err)
	}
	if deleteCalls != 1 || identityCalls != 1 {
		t.Fatalf("calls DELETE=%d identity=%d, want 1 each", deleteCalls, identityCalls)
	}
}
