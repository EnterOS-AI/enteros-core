package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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

// TestHandle_DeniesNonWebPorts verifies the proxy is not a general TCP relay:
// CONNECT is limited to 443 and plain HTTP to 80, so a compromised sidecar
// cannot tunnel to SSH/SMTP/arbitrary C2 ports even on a public IP. The port is
// vetted before any DNS/dial, so these return 403 without touching the network.
func TestHandle_DeniesNonWebPorts(t *testing.T) {
	// CONNECT to a PUBLIC IP on :22 → 403 (port, not IP).
	t.Run("connect-public-ssh", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://example", nil)
		req.Host = "1.1.1.1:22"
		req.URL.Host = "1.1.1.1:22"
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("CONNECT 1.1.1.1:22: got %d, want 403", rec.Code)
		}
	})
	// Plain HTTP to a public IP on :8080 → 403 (port).
	t.Run("http-public-nonstandard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://1.1.1.1:8080/x", nil)
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET 1.1.1.1:8080: got %d, want 403", rec.Code)
		}
	})
}

// TestPortAllowlist unit-tests the port allowlist helpers directly: this is the
// check that keeps the proxy an HTTPS/HTTP web egress rather than a general TCP
// relay. The full CONNECT-443 allow path (which requires a live public endpoint
// and a real dial) is covered by the live-container e2e, as with the deny tests.
func TestPortAllowlist(t *testing.T) {
	if !connectPortAllowed("443") {
		t.Error("connectPortAllowed(443) = false, want true (HTTPS must pass)")
	}
	for _, p := range []string{"22", "80", "25", "8443", ""} {
		if connectPortAllowed(p) {
			t.Errorf("connectPortAllowed(%q) = true, want false", p)
		}
	}
	if !httpPortAllowed("80") {
		t.Error("httpPortAllowed(80) = false, want true (HTTP must pass)")
	}
	for _, p := range []string{"443", "8080", "22", ""} {
		if httpPortAllowed(p) {
			t.Errorf("httpPortAllowed(%q) = true, want false", p)
		}
	}
}

// tcpPair returns a connected pair of real TCP sockets (so CloseWrite / EOF
// half-close semantics behave like production, unlike net.Pipe).
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	return dialed, r.c
}

// TestTunnel_NoTruncationBidirectional exercises tunnel with a large payload in
// each direction at once. The old teardown (block on a single done, then close
// both sockets) truncated whichever direction was still draining; tunnel must
// deliver both payloads in full. Sizes exceed the socket buffers so the copies
// genuinely overlap in time.
func TestTunnel_NoTruncationBidirectional(t *testing.T) {
	clientOuter, clientInner := tcpPair(t) // test writes/reads clientOuter; tunnel uses clientInner
	upstreamInner, upstreamOuter := tcpPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tunnel(ctx, clientInner, upstreamInner)

	const n = 4 << 20 // 4 MiB, larger than any socket buffer
	upload := make([]byte, n)
	download := make([]byte, n)
	if _, err := rand.Read(upload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(download); err != nil {
		t.Fatalf("rand: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// client -> upstream (upload), then half-close.
	go func() {
		defer wg.Done()
		if _, err := clientOuter.Write(upload); err != nil {
			t.Errorf("write upload: %v", err)
		}
		clientOuter.(*net.TCPConn).CloseWrite()
	}()
	// upstream -> client (download), then half-close.
	go func() {
		defer wg.Done()
		if _, err := upstreamOuter.Write(download); err != nil {
			t.Errorf("write download: %v", err)
		}
		upstreamOuter.(*net.TCPConn).CloseWrite()
	}()

	gotUpload, err := io.ReadAll(upstreamOuter)
	if err != nil {
		t.Fatalf("read upload: %v", err)
	}
	gotDownload, err := io.ReadAll(clientOuter)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	wg.Wait()

	if !bytes.Equal(gotUpload, upload) {
		t.Errorf("upload truncated/corrupted: got %d bytes, want %d", len(gotUpload), len(upload))
	}
	if !bytes.Equal(gotDownload, download) {
		t.Errorf("download truncated/corrupted: got %d bytes, want %d", len(gotDownload), len(download))
	}
}
