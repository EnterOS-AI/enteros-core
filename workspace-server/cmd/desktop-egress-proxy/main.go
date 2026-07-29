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

// httpsPort / httpPort are the ONLY destination ports the proxy will reach. It
// exists to permit web browsing, not to be a general TCP relay, so CONNECT
// tunnels are limited to HTTPS (443) and plain-HTTP proxying to 80. A
// compromised sidecar therefore cannot tunnel to SSH/SMTP or an arbitrary C2
// port even though the destination IP is a public one.
const (
	httpsPort = "443"
	httpPort  = "80"
)

// connectPortAllowed reports whether a CONNECT tunnel to port is permitted.
func connectPortAllowed(port string) bool { return port == httpsPort }

// httpPortAllowed reports whether a plain-HTTP proxied request to port is
// permitted.
func httpPortAllowed(port string) bool { return port == httpPort }

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
	if !connectPortAllowed(port) {
		log.Printf("DENY CONNECT %s: port %s not permitted (only %s)", r.Host, port, httpsPort)
		http.Error(w, "destination port not permitted", http.StatusForbidden)
		return
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
	tunnel(r.Context(), client, upstream)
}

// tunnel bidirectionally copies between the client and upstream connections
// until BOTH directions have finished. When one direction's reader hits EOF it
// half-closes the peer's write end (CloseWrite) so the peer sees EOF without the
// other direction being torn down — this is what stops a large upload still
// draining after the download completes from being truncated (the previous
// single-<-done teardown closed both sockets the instant the first copy
// finished). The request context is bound so a client disconnect force-closes
// both ends and neither goroutine can block forever on a peer that never sends
// EOF.
func tunnel(ctx context.Context, client, upstream net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite() // signal EOF to the peer, keep its reader alive
		}
		done <- struct{}{}
	}
	go func() {
		<-ctx.Done()
		client.Close()
		upstream.Close()
	}()
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
	<-done
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
	port := r.URL.Port()
	if port == "" {
		port = httpPort
	}
	if !httpPortAllowed(port) {
		log.Printf("DENY %s %s: port %s not permitted (only %s)", r.Method, r.URL, port, httpPort)
		http.Error(w, "destination port not permitted", http.StatusForbidden)
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
				port = "80"
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
