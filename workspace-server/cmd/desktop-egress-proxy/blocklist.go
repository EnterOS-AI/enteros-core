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
	if ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (metadata + host gw), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	// Defense-in-depth against IPv6 transition mechanisms that embed an IPv4
	// address: NAT64 (64:ff9b::/96) and 6to4 (2002::/16) can smuggle a private
	// v4 (e.g. NAT64 of 169.254.169.254 = 64:ff9b::a9fe:a9fe) that the checks
	// above miss because the OUTER v6 address is "public". If the embedded v4 is
	// itself blocked, block the whole address. (v4-mapped ::ffff:0:0/96 is
	// already handled by To4() inside IsPrivate/IsLinkLocalUnicast above.)
	if v4 := embeddedV4(ip); v4 != nil && isBlockedIP(v4) {
		return true
	}
	return false
}

// embeddedV4 returns the IPv4 address embedded in an IPv6 transition-mechanism
// address (NAT64 64:ff9b::/96 or 6to4 2002::/16), or nil.
func embeddedV4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return nil // not v6, or already a v4 form
	}
	// NAT64 well-known prefix 64:ff9b::/96 → last 4 bytes are the v4.
	if v6[0] == 0x00 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b &&
		v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
		v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	// 6to4 2002::/16 → bytes 2..5 are the v4.
	if v6[0] == 0x20 && v6[1] == 0x02 {
		return net.IPv4(v6[2], v6[3], v6[4], v6[5])
	}
	return nil
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
