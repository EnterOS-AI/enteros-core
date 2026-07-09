package provisioner

import "testing"

// isLoopbackHostURL gates whether a configured LANGFUSE_HOST is rewritten to the
// container-network URL. A host-loopback value (the platform's host-published
// Langfuse) is unreachable from a sibling workspace container and MUST rewrite;
// a real external target MUST be preserved.
func TestIsLoopbackHostURL(t *testing.T) {
	loopback := []string{
		"http://127.0.0.1:3001",
		"http://localhost:3001",
		"https://localhost",
		"http://[::1]:3001",
		"http://127.0.0.5:8080", // 127/8 is all loopback
	}
	for _, u := range loopback {
		if !isLoopbackHostURL(u) {
			t.Errorf("isLoopbackHostURL(%q) = false, want true (must rewrite to container URL)", u)
		}
	}
	external := []string{
		"http://langfuse-web:3000", // the container-network target itself
		"https://cloud.langfuse.com",
		"http://langfuse.internal.example.com",
		"", // unset — handled by the !set branch, not the loopback check
	}
	for _, u := range external {
		if isLoopbackHostURL(u) {
			t.Errorf("isLoopbackHostURL(%q) = true, want false (deliberate target, preserve)", u)
		}
	}
}
