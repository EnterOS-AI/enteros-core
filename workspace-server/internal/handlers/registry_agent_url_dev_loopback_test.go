package handlers

import (
	"strings"
	"testing"
)

// TestValidateAgentURLDevLoopback pins the MOLECULE_ENV=development loopback
// carve-out in validateAgentURL (the register-time mirror of ssrf.go's
// devModeAllowsLoopback): on a local dev host the provisioner itself assigns
// http://127.0.0.1:<hostport> advertise URLs, so the validator must accept
// them — while every metadata/documentation range stays blocked even in dev,
// and loopback stays blocked outside dev.
func TestValidateAgentURLDevLoopback(t *testing.T) {
	cases := []struct {
		name    string
		env     string // MOLECULE_ENV
		url     string
		wantErr string // "" = must pass; else substring of the error
	}{
		// The observed local-dev failure: provisioner-assigned advertise URL.
		{"dev allows IPv4 loopback", "development", "http://127.0.0.1:63026", ""},
		{"dev alias allows IPv4 loopback", "dev", "http://127.0.0.1:63026", ""},
		{"dev allows IPv6 loopback", "development", "http://[::1]:63026", ""},
		{"dev allows loopback range", "development", "http://127.0.0.99:8000", ""},

		// Dev mode must NOT widen anything but loopback.
		{"dev still blocks IMDS", "development", "http://169.254.169.254/latest", "link-local"},
		{"dev still blocks TEST-NET", "development", "http://192.0.2.10:8000", "TEST-NET"},
		{"dev still blocks CGNAT", "development", "http://100.64.0.1:8000", "carrier-grade"},
		{"dev still blocks RFC-1918", "development", "http://10.1.2.3:8000", "RFC-1918"},

		// Outside dev mode loopback stays blocked (SaaS and self-hosted prod).
		{"prod blocks IPv4 loopback", "production", "http://127.0.0.1:63026", "loopback"},
		{"prod blocks IPv6 loopback", "production", "http://[::1]:63026", "IPv6 loopback"},
		{"unset env blocks loopback", "", "http://127.0.0.1:63026", "loopback"},

		// The long-standing name exemption is mode-independent.
		{"localhost by name allowed in prod", "production", "http://localhost:3000", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_ENV", tc.env)
			err := validateAgentURL(tc.url)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAgentURL(%q) in env %q: unexpected error %v", tc.url, tc.env, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAgentURL(%q) in env %q: want error containing %q, got nil", tc.url, tc.env, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateAgentURL(%q) in env %q: want error containing %q, got %v", tc.url, tc.env, tc.wantErr, err)
			}
		})
	}
}
