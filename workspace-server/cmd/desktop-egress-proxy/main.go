// Command desktop-egress-proxy is the desktop sidecar's egress proxy: a small
// HTTP/CONNECT forward proxy that permits the public internet but DENIES every
// private / link-local / loopback destination (see blocklist.go).
//
// It is the structural isolation boundary for the agent desktop (design §6.1):
// the sidecar runs on an INTERNAL Docker network with no egress of its own, so
// its browser can reach nothing except this proxy. Because the proxy refuses to
// forward to RFC-1918 / 169.254 / loopback, a compromised sidecar cannot reach
// backend infra (Postgres/Redis/MinIO/LiteLLM), the Docker host, other tenants,
// or the cloud metadata service — with NO host firewall and NO operator step.
// This is what lets the desktop feature be on by default and safe by
// construction. Runs in its own container (wsdeskproxy-<id>), dual-homed on the
// per-workspace internal net (where it accepts the sidecar's traffic) and an
// egress network (its only route out).
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

const dialTimeout = 15 * time.Second

// httpsPort / httpPort are the DEFAULT destination ports assumed when a request
// omits one (a bare CONNECT host, or an absolute-form http:// URL without a
// port). The proxy does NOT restrict which port it will reach: the security
// boundary is the IP blocklist (private/link-local/loopback/CGNAT denied), and a
// port allowlist adds no real isolation — HTTPS-based C2/exfil rides 443, which
// any web proxy must allow, so narrowing to 443/80 only broke legitimate sites
// served on alt ports (e.g. https://host:8443, http://host:8080) without
// containing an attacker (verified 2026-07-28).
const (
	httpsPort = "443"
	httpPort  = "80"
)

func main() {
	addr := os.Getenv("DESKTOP_PROXY_ADDR")
	if addr == "" {
		addr = ":3128"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("desktop-egress-proxy listening on %s (public internet allowed; private/link-local/loopback denied)", addr)
	log.Fatal(srv.ListenAndServe())
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		handleConnect(w, r)
		return
	}
	handleHTTP(w, r)
}

// handleConnect tunnels HTTPS (and any TCP) via the CONNECT method, after
// vetting the destination.
func handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, httpsPort
	}
	ip, err := resolveAllowedIP(host)
	if err != nil {
		log.Printf("DENY CONNECT %s: %v", r.Host, err)
		http.Error(w, "destination not permitted", http.StatusForbidden)
		return
	}
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), dialTimeout)
	if err != nil {
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	tunnel(client, upstream, tunnelDrainTimeout)
}

// tunnelDrainTimeout bounds how long the SECOND tunnel direction may keep
// copying after the FIRST has finished and half-closed its peer. It is an
// ABSOLUTE cap measured from that first half-close — a backstop against a
// hung/malicious peer that never sends EOF, not an idle timeout. 60s is generous
// enough that normal request/response traffic (where a peer seeing EOF closes in
// milliseconds) completes well within it; the tradeoff is that a transfer which
// keeps streaming for >60s AFTER its peer half-closed one direction would be cut.
// That is rare and strictly preferable to the unbounded goroutine/fd leak it
// replaces. Passed into tunnel as a parameter (not read from a mutable global) so
// tests can shorten it without a data race against a live tunnel goroutine.
const tunnelDrainTimeout = 60 * time.Second

// tunnel bidirectionally copies between the client and upstream connections.
// When one direction's reader hits EOF it half-closes the peer's write end
// (CloseWrite) so the peer sees EOF WITHOUT the other direction being torn down
// — this stops a large upload still draining after the download completes from
// being truncated (the older single-<-done teardown closed both sockets the
// instant the first copy finished).
//
// Leak safety (verified 2026-07-28): this runs on a HIJACKED CONNECT connection,
// whose request context is NOT cancelled until ServeHTTP returns — and ServeHTTP
// is blocked right here in tunnel — so a ctx-done closer goroutine could never
// fire mid-tunnel. Relying on it let a peer that never sends EOF block the second
// io.Copy (and both fds + the goroutine) FOREVER. Instead, once the first
// direction completes we put a drain DEADLINE (drainTimeout) on both conns,
// turning that unbounded leak into a bounded wait, then force both closed.
func tunnel(client, upstream net.Conn, drainTimeout time.Duration) {
	type closeWriter interface{ CloseWrite() error }
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite() // signal EOF to the peer, keep its reader alive
		}
		done <- struct{}{}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)

	<-done // one direction finished and half-closed its peer's write side
	// Bound the still-open direction so a peer that never EOFs cannot hang it (and
	// this goroutine + both sockets) indefinitely.
	deadline := time.Now().Add(drainTimeout)
	_ = client.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	<-done

	client.Close()
	upstream.Close()
}

// hopByHopHeaders are stripped when forwarding a plain-HTTP proxied request.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// handleHTTP forwards a plain-HTTP proxied request (absolute-form URI), dialing
// the pre-vetted IP so the connection cannot be rebound to a private address
// between the check and the dial.
func handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy requires an absolute-form request URI", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	ip, err := resolveAllowedIP(host)
	if err != nil {
		log.Printf("DENY %s %s: %v", r.Method, r.URL, err)
		http.Error(w, "destination not permitted", http.StatusForbidden)
		return
	}
	// A transport that ALWAYS dials the vetted IP (preserving the port), never
	// re-resolving the host.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				port = httpPort
			}
			d := &net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
	}
	defer transport.CloseIdleConnections()

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	for _, h := range hopByHopHeaders {
		outReq.Header.Del(h)
	}
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopByHopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
