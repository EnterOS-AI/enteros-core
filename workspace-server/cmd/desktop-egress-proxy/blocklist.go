package main

import (
	"fmt"
	"net"
)

// isBlockedIP reports whether an IP must NOT be reachable through the desktop
// egress proxy. It blocks everything that is not a globally-routable public
// address: RFC-1918 private ranges (10/8, 172.16/12, 192.168/16, fc00::/7),
// link-local (169.254/16 + fe80::/10 — this is the cloud metadata IP
// 169.254.169.254 AND the Docker host gateway), loopback, unspecified, and
// multicast. This is the enforcement point that makes the desktop sidecar's
// isolation structural: the sidecar sits on an internal Docker network with no
// egress of its own and can ONLY reach this proxy, so denying private
// destinations here means a compromised browser cannot reach backend infra
// (Postgres/Redis/MinIO/LiteLLM), the host, other tenants, or steal cloud
// credentials — with no host firewall and no operator step.
func isBlockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (metadata + host gw), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// resolveAllowedIP resolves host and returns ONE public IP safe to dial, or an
// error if the host is missing or ANY resolved address is blocked. Failing
// closed on a mixed result (a name that resolves to both public and private
// IPs) defeats DNS-rebinding: an attacker cannot smuggle a private destination
// behind a name that also has a public record. The caller dials the returned
// vetted IP directly (never re-resolves), so there is no TOCTOU window between
// the check and the connect.
func resolveAllowedIP(host string) (net.IP, error) {
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	// A literal IP is checked directly (no DNS).
	if lit := net.ParseIP(host); lit != nil {
		if isBlockedIP(lit) {
			return nil, fmt.Errorf("destination %s is a blocked (private/link-local/loopback) address", host)
		}
		return lit, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %q: no addresses", host)
	}
	var allowed net.IP
	for _, ip := range ips {
		if isBlockedIP(ip) {
			// Fail closed on ANY blocked address in the set.
			return nil, fmt.Errorf("destination %q resolves to a blocked address %s", host, ip)
		}
		if allowed == nil {
			allowed = ip
		}
	}
	return allowed, nil
}
