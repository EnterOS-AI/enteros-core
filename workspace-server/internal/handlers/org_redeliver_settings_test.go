package handlers

// Contract for re-delivering declared plugin settings on org import.
//
// The property that matters is NOT "does deliverPluginSettings work" — that is
// covered elsewhere. It is: does the ON CONFLICT skip branch reach delivery at
// all. Before this, an existing workspace returned "skipped": true and the
// config wiring was never reached, so the declarations were unreachable by any
// route. A test of the delivery primitive cannot detect that; only a test of
// the skip path can.

import (
	"context"
	"errors"
	"testing"
)

type fakeDeliverer struct {
	calls  []string                 // installName order
	config map[string]any           // last settings seen
	err    error
	reload bool
}

func (f *fakeDeliverer) deliverPluginSettings(_ context.Context, _, installName string,
	settings map[string]any) (bool, error) {
	f.calls = append(f.calls, installName)
	f.config = settings
	return f.reload, f.err
}

const schedSrc = "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"

func TestRedeliver_DeliversDeclaredConfig(t *testing.T) {
	f := &fakeDeliverer{reload: true}
	h := (&OrgHandler{}).WithPluginSettingsDeliverer(f)
	n := h.redeliverDeclaredPluginSettings(context.Background(), "ws-1", "Coordinator",
		[]templatePluginEntry{{Source: schedSrc, Config: map[string]any{"schedules": []any{map[string]any{"name": "Heartbeat"}}}}})
	if n != 1 {
		t.Fatalf("expected 1 delivery, got %d", n)
	}
	// Keyed on the INSTALL name (repo), never the manifest name — the manifest
	// name writes a file nothing opens, and fails silently.
	if len(f.calls) != 1 || f.calls[0] != "molecule-ai-plugin-scheduler" {
		t.Fatalf("wrong install name: %v", f.calls)
	}
	if _, ok := f.config["schedules"]; !ok {
		t.Fatalf("config not forwarded: %v", f.config)
	}
}

func TestRedeliver_NilDelivererIsInert(t *testing.T) {
	// THE SAFETY PROPERTY. With nothing wired, org import must behave exactly
	// as before — this is what makes the change opt-in at the router.
	h := &OrgHandler{}
	if n := h.redeliverDeclaredPluginSettings(context.Background(), "ws-1", "X",
		[]templatePluginEntry{{Source: schedSrc, Config: map[string]any{"a": 1}}}); n != 0 {
		t.Fatalf("nil deliverer must deliver nothing, got %d", n)
	}
}

func TestRedeliver_EntriesWithoutConfigAreSkipped(t *testing.T) {
	f := &fakeDeliverer{}
	h := (&OrgHandler{}).WithPluginSettingsDeliverer(f)
	n := h.redeliverDeclaredPluginSettings(context.Background(), "ws-1", "X",
		[]templatePluginEntry{{Source: schedSrc}, {Source: "ecc"}})
	if n != 0 || len(f.calls) != 0 {
		t.Fatalf("string-form entries carry no settings; got n=%d calls=%v", n, f.calls)
	}
}

func TestRedeliver_OptOutMarkersAreNotDelivered(t *testing.T) {
	// "!name" / "-name" are REMOVALS in the merge grammar. Delivering settings
	// for a plugin the node declined would resurrect it by the back door.
	f := &fakeDeliverer{}
	h := (&OrgHandler{}).WithPluginSettingsDeliverer(f)
	for _, src := range []string{"!" + schedSrc, "-" + schedSrc} {
		if n := h.redeliverDeclaredPluginSettings(context.Background(), "ws-1", "X",
			[]templatePluginEntry{{Source: src, Config: map[string]any{"a": 1}}}); n != 0 {
			t.Fatalf("opt-out %q must not deliver", src)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("opt-out delivered: %v", f.calls)
	}
}

func TestRedeliver_OneFailureDoesNotDropSiblings(t *testing.T) {
	f := &fakeDeliverer{err: errors.New("boom")}
	h := (&OrgHandler{}).WithPluginSettingsDeliverer(f)
	n := h.redeliverDeclaredPluginSettings(context.Background(), "ws-1", "X",
		[]templatePluginEntry{
			{Source: schedSrc, Config: map[string]any{"a": 1}},
			{Source: "gitea://molecule-ai/molecule-ai-plugin-seo#v1", Config: map[string]any{"b": 2}},
		})
	if n != 0 {
		t.Fatalf("all deliveries errored; expected 0 counted, got %d", n)
	}
	if len(f.calls) != 2 {
		t.Fatalf("a failure must not abort the remaining entries: %v", f.calls)
	}
}

func TestRedeliver_EmptyEntriesIsNoOp(t *testing.T) {
	f := &fakeDeliverer{}
	h := (&OrgHandler{}).WithPluginSettingsDeliverer(f)
	if n := h.redeliverDeclaredPluginSettings(context.Background(), "ws", "X", nil); n != 0 || len(f.calls) != 0 {
		t.Fatalf("no entries must be a no-op")
	}
}

func TestRedeliver_TemplatesHandlerSatisfiesTheInterface(t *testing.T) {
	// Compile-time proof the router wiring is legal without threading the whole
	// struct through org import.
	var _ pluginSettingsDeliverer = (*TemplatesHandler)(nil)
}
