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
		host, port = r.Host, "443"
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
	// Pipe both directions until either side closes.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
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
