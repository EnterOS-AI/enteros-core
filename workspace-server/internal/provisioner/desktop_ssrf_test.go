package provisioner

import "testing"

func TestValidateSidecarTarget_AllowsLegitimateSidecars(t *testing.T) {
	good := []string{
		"wsdesk-abc12345-6789-4def-8123-56789abcdef0:6070", // control server
		"wsdesk-abc12345-6789-4def-8123-56789abcdef0:6080", // noVNC
		DesktopContainerName("deadbeef") + ":6070",
	}
	for _, h := range good {
		if err := ValidateSidecarTarget(h); err != nil {
			t.Errorf("ValidateSidecarTarget(%q) = %v, want nil", h, err)
		}
	}
}

func TestValidateSidecarTarget_RejectsSSRFAttempts(t *testing.T) {
	bad := []string{
		"",                                      // empty
		"wsdesk-abc",                            // no port
		"wsdesk-abc:",                           // empty port
		"wsdesk-abc:80",                         // disallowed port
		"wsdesk-abc:443",                        // disallowed port
		"wsdesk-abc:22",                         // disallowed port
		"169.254.169.254:6070",                  // metadata IP, not a sidecar
		"postgres:6070",                         // backend service name
		"evil.com:6080",                         // arbitrary external host
		"ws-abc:6070",                           // tenant container, not a sidecar
		"wsdesk-:6070",                          // empty id
		"wsdesk-abc/../evil:6070",               // path-ish id
		"wsdesk-abc@evil.com:6070",              // userinfo-style injection
		"wsdesk-abc.evil.com:6080",              // dotted host injection
		"wsdesk-abc12345-6789-4def:6070:extra",  // extra colon segment lands in port
	}
	for _, h := range bad {
		if err := ValidateSidecarTarget(h); err == nil {
			t.Errorf("ValidateSidecarTarget(%q) = nil, want rejection", h)
		}
	}
}

func TestIsWellFormedWorkspaceID(t *testing.T) {
	if !IsWellFormedWorkspaceID("abc12345-6789-4def-8123-56789abcdef0") {
		t.Error("well-formed UUID rejected")
	}
	for _, bad := range []string{"", "abc/def", "abc.def", "abc@x", "abc:1", "g123", "abc def"} {
		if IsWellFormedWorkspaceID(bad) {
			t.Errorf("IsWellFormedWorkspaceID(%q) = true, want false", bad)
		}
	}
}
