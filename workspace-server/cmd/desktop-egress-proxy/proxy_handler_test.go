package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

// TestHandle_BlocksBlockedIPRegardlessOfPort verifies the security boundary is
// the IP blocklist and is PORT-INDEPENDENT: a blocked (private/metadata)
// destination is refused with 403 before any dial on a non-standard port just as
// on 443/80. The proxy no longer restricts by port (a port allowlist added no
// isolation — HTTPS C2/exfil rides 443 anyway — while breaking legitimate
// alt-port sites like :8443), so the IP check must carry the boundary alone.
func TestHandle_BlocksBlockedIPRegardlessOfPort(t *testing.T) {
	// CONNECT to a PRIVATE IP on a non-standard port → still 403 (IP, port-agnostic).
	t.Run("connect-private-altport", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://example", nil)
		req.Host = "10.0.0.1:8443"
		req.URL.Host = "10.0.0.1:8443"
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("CONNECT 10.0.0.1:8443: got %d, want 403", rec.Code)
		}
	})
	// Plain HTTP to a private IP on a non-standard port → still 403.
	t.Run("http-private-altport", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.1:8080/x", nil)
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET 192.168.1.1:8080: got %d, want 403", rec.Code)
		}
	})
	// Metadata IP on a non-standard port → still 403.
	t.Run("connect-metadata-altport", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://example", nil)
		req.Host = "169.254.169.254:9000"
		req.URL.Host = "169.254.169.254:9000"
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("CONNECT metadata:9000: got %d, want 403", rec.Code)
		}
	})
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

	go tunnel(clientInner, upstreamInner)

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

// TestTunnel_HungPeerDoesNotBlockForever is the regression guard for the
// fd/goroutine leak (verified 2026-07-28): on a hijacked CONNECT the request
// context is never cancelled while ServeHTTP is blocked in tunnel, so a peer that
// half-closes one way but never sends EOF the other way must NOT hang tunnel
// forever — the drain deadline has to force it closed. Here the client finishes
// and half-closes, but the upstream never writes and never closes; tunnel must
// still return within the (shortened) drain window.
func TestTunnel_HungPeerDoesNotBlockForever(t *testing.T) {
	orig := tunnelDrainTimeout
	tunnelDrainTimeout = 200 * time.Millisecond
	defer func() { tunnelDrainTimeout = orig }()

	clientOuter, clientInner := tcpPair(t)
	upstreamInner, upstreamOuter := tcpPair(t)
	defer upstreamOuter.Close()

	done := make(chan struct{})
	go func() {
		tunnel(clientInner, upstreamInner)
		close(done)
	}()

	// Client sends a little then half-closes its write side (its reader stays open
	// waiting for a response that never comes).
	if _, err := clientOuter.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	clientOuter.(*net.TCPConn).CloseWrite()
	// upstreamOuter deliberately never writes and never closes — the hung peer.

	select {
	case <-done:
		// tunnel returned — the drain deadline unblocked the stuck direction.
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel did not return: a hung upstream leaked the goroutine + fds (drain deadline ineffective)")
	}
}
