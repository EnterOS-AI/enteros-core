package handlers

import (
	"context"
	"net/url"
	"testing"
)

// The re-homed transport hands the reverse proxy an http://<sidecar>:6080
// target directly — no EC2 tunnel (§13).
func TestDesktopDisplayForward_DialsSidecarDirectly(t *testing.T) {
	var got *url.URL
	err := desktopDisplayForward(context.Background(), "wsdesk-w1:6080", func(target *url.URL) error {
		got = target
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Scheme != "http" || got.Host != "wsdesk-w1:6080" {
		t.Fatalf("target = %+v, want http://wsdesk-w1:6080", got)
	}
}
