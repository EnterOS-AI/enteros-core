package main

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"10.0.0.1",               // RFC-1918
		"172.16.5.4",             // RFC-1918
		"172.31.255.255",         // RFC-1918 upper
		"192.168.1.1",            // RFC-1918
		"169.254.169.254",        // cloud metadata
		"169.254.0.1",            // link-local (docker host gw range)
		"127.0.0.1",              // loopback
		"0.0.0.0",                // unspecified
		"::1",                    // v6 loopback
		"fe80::1",                // v6 link-local
		"fc00::1",                // v6 ULA (private)
		"224.0.0.1",              // multicast
		"::ffff:169.254.169.254", // v4-mapped metadata
		"::ffff:10.0.0.1",        // v4-mapped private
		"64:ff9b::a9fe:a9fe",     // NAT64 of metadata 169.254.169.254
		"64:ff9b::a00:1",         // NAT64 of 10.0.0.1
		"2002:a9fe:a9fe::1",      // 6to4 embedding 169.254.169.254
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) = false, want true (must be denied)", s)
		}
	}
	allowed := []string{
		"1.1.1.1",              // public
		"8.8.8.8",              // public
		"93.184.216.34",        // example.com
		"172.15.0.1",           // just below RFC-1918 172.16/12
		"172.32.0.1",           // just above RFC-1918 172.16/12
		"2606:4700:4700::1111", // public v6
	}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) = true, want false (public must be allowed)", s)
		}
	}
	// A nil/garbage IP is blocked (fail closed).
	if !isBlockedIP(net.ParseIP("not-an-ip")) {
		t.Error("unparseable IP must be blocked")
	}
}

func TestResolveAllowedIP_LiteralAddresses(t *testing.T) {
	// Literal public IP passes without DNS.
	if ip, err := resolveAllowedIP("1.1.1.1"); err != nil || ip.String() != "1.1.1.1" {
		t.Fatalf("resolveAllowedIP(1.1.1.1) = (%v, %v), want (1.1.1.1, nil)", ip, err)
	}
	// Literal private / metadata IP is denied.
	for _, s := range []string{"10.0.0.1", "169.254.169.254", "192.168.1.1", "127.0.0.1"} {
		if _, err := resolveAllowedIP(s); err == nil {
			t.Errorf("resolveAllowedIP(%s) = nil error, want denied", s)
		}
	}
	// Empty host denied.
	if _, err := resolveAllowedIP(""); err == nil {
		t.Error("empty host must be denied")
	}
}
