package handlers

import (
	"encoding/json"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/messagestore"
)

// selfsource_wake_bodies_test.go — pins the live-bug fix that stall-probe and
// inbox-nudge self-wakes must carry (a) a platform self-source source_type so
// they classify as system notices instead of leaking as blue user bubbles, and
// (b) a deterministic contextId so they land in the workspace's default session
// instead of minting a fresh runtime session (fragmentation).

// wakeMessageMeta extracts params.message.{contextId,metadata} from a built
// message/send body, failing the test if the shape is wrong.
func wakeMessageMeta(t *testing.T, body []byte) (contextID string, meta map[string]any) {
	t.Helper()
	var parsed struct {
		Params struct {
			Message struct {
				ContextID string         `json:"contextId"`
				Metadata  map[string]any `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	return parsed.Params.Message.ContextID, parsed.Params.Message.Metadata
}

func TestBuildStallProbeBody_CarriesSelfSourceAndContextID(t *testing.T) {
	const wsID = "ws-stall-1"
	body, err := buildStallProbeBody(wsID, 30, 10)
	if err != nil {
		t.Fatalf("buildStallProbeBody: %v", err)
	}

	ctxID, meta := wakeMessageMeta(t, body)
	if ctxID == "" {
		t.Errorf("stall probe missing contextId — a self-wake without one mints a fresh runtime session (fragmentation)")
	}
	if want := platformTurnContextID(wsID); ctxID != want {
		t.Errorf("stall probe contextId=%q, want deterministic default-session id %q", ctxID, want)
	}
	if meta == nil {
		t.Fatalf("stall probe missing message.metadata")
	}
	if meta["source"] != "platform" {
		t.Errorf("stall probe metadata.source=%v, want platform", meta["source"])
	}
	st, _ := meta["source_type"].(string)
	if st != "self-stall" {
		t.Errorf("stall probe metadata.source_type=%q, want self-stall", st)
	}
	if !messagestore.IsSelfSourceType(st) {
		t.Errorf("source_type %q not in messagestore.selfSourceTypes — it will still leak as a blue user bubble", st)
	}
}

func TestBuildNudgeBody_CarriesSelfSourceAndContextID(t *testing.T) {
	const wsID = "ws-nudge-1"
	body, err := buildNudgeBody(wsID, 3)
	if err != nil {
		t.Fatalf("buildNudgeBody: %v", err)
	}

	ctxID, meta := wakeMessageMeta(t, body)
	if ctxID == "" {
		t.Errorf("inbox nudge missing contextId — a self-wake without one mints a fresh runtime session (fragmentation)")
	}
	if want := platformTurnContextID(wsID); ctxID != want {
		t.Errorf("inbox nudge contextId=%q, want deterministic default-session id %q", ctxID, want)
	}
	if meta == nil {
		t.Fatalf("inbox nudge missing message.metadata")
	}
	if meta["source"] != "platform" {
		t.Errorf("inbox nudge metadata.source=%v, want platform", meta["source"])
	}
	st, _ := meta["source_type"].(string)
	if st != "self-nudge" {
		t.Errorf("inbox nudge metadata.source_type=%q, want self-nudge", st)
	}
	if !messagestore.IsSelfSourceType(st) {
		t.Errorf("source_type %q not in messagestore.selfSourceTypes — it will still leak as a blue user bubble", st)
	}
}
