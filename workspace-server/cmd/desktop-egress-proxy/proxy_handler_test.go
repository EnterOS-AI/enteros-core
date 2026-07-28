package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandle_DeniesPrivateDestinations verifies the HTTP surface refuses
// private / metadata destinations with 403 BEFORE any dial — the deny path that
// keeps a compromised browser off backend infra. (The allow path needs a real
// public endpoint and is covered by the live-container e2e.)
func TestHandle_DeniesPrivateDestinations(t *testing.T) {
	// CONNECT to a private IP → 403 (returned before hijack).
	t.Run("connect-private", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://example", nil)
		req.Host = "10.0.0.1:443"
		req.URL.Host = "10.0.0.1:443"
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("CONNECT 10.0.0.1: got %d, want 403", rec.Code)
		}
	})
	// CONNECT to the metadata IP → 403.
	t.Run("connect-metadata", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://example", nil)
		req.Host = "169.254.169.254:80"
		req.URL.Host = "169.254.169.254:80"
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("CONNECT metadata: got %d, want 403", rec.Code)
		}
	})
	// Plain HTTP (absolute form) to a private IP → 403.
	t.Run("http-private", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.1/admin", nil)
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET 192.168.1.1: got %d, want 403", rec.Code)
		}
	})
	// Non-absolute request URI → 400 (a proxy only serves absolute-form).
	t.Run("http-non-absolute", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/relative", nil)
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("relative URI: got %d, want 400", rec.Code)
		}
	})
}
