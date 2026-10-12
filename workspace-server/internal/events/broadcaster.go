package events

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/ws"
	"github.com/redis/go-redis/v9"
)

const broadcastChannel = "events:broadcast"

// EventEmitter is the contract handler code needs from a broadcaster.
// Defining it here lets tests substitute a capture-only stub instead
// of standing up the full Redis + WebSocket hub topology that the
// concrete *Broadcaster builds (and that previously blocked
// TestProvisionWorkspace_* regression tests on issue #1814).
//
// Includes BroadcastOnly because the activity-log + A2A-response paths
// inside the handler package fan out via that method — narrowing
// further would force production callers back to the concrete type.
//
// *Broadcaster satisfies this interface trivially. Production code that
// needs the wider surface (SubscribeSSE, Subscribe) keeps using the
// concrete *Broadcaster type — sse.go + cmd/server/main.go are the
// only such call sites today.
type EventEmitter interface {
	RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error
	BroadcastOnly(workspaceID string, eventType string, payload interface{})
}

// Compile-time assertion: a renamed/reshaped Broadcaster method that
// silently broke this interface would fail to build here.
var _ EventEmitter = (*Broadcaster)(nil)

// sseSubscription is a single in-process SSE subscriber.
// deliverToSSE writes to ch; StreamEvents reads from it.
type sseSubscription struct {
	workspaceID string
	ch          chan models.WSMessage
}

type Broadcaster struct {
	hub    *ws.Hub
	ssesMu sync.RWMutex
	sses   []*sseSubscription
	// bootTrace mirrors the BOOT_STEP stream (and the lifecycle events that
	// reset it) so GET /workspaces can replay a mid-boot history to a
	// re-hydrating canvas. See boottrace.go.
	bootTrace *BootTrace
}

func NewBroadcaster(hub *ws.Hub) *Broadcaster {
	return &Broadcaster{hub: hub, bootTrace: NewBootTrace()}
}

// BootStepsFor exposes the observed boot-telemetry history for a workspace.
// Consumed by the workspaces List handler through a narrow local interface
// assertion (not EventEmitter) so test fakes are unaffected.
func (b *Broadcaster) BootStepsFor(workspaceID string) []BootStepRecord {
	return b.bootTrace.StepsFor(workspaceID)
}

// BootReplayFor exposes the observed boot history WITH its generation marker
// (see BootTrace.Replay) — the List handler serves both so the canvas can
// tell "same boot, merge" from "new boot, reset".
func (b *Broadcaster) BootReplayFor(workspaceID string) ([]BootStepRecord, string, bool) {
	return b.bootTrace.Replay(workspaceID)
}

// RecordAndBroadcast inserts a structure event into Postgres and publishes to Redis pub/sub.
func (b *Broadcaster) RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Insert into structure_events — cast to jsonb explicitly
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, workspace_id, payload)
		VALUES ($1, $2, $3::jsonb)
	`, eventType, workspaceID, string(payloadJSON))
	if err != nil {
		log.Printf("RecordAndBroadcast: insert event error: %v", err)
		return err
	}

	// Boot-telemetry lifecycle tap (boottrace.go): a WORKSPACE_PROVISIONING
	// starts a new boot generation; ONLINE/PROVISION_FAILED/REMOVED free the
	// entry. Deliberately AFTER the persist — an aborted record (DB error
	// above) must not wipe a live boot's trace for an event that never
	// actually happened.
	b.bootTrace.Observe(eventType, workspaceID, payload)

	// Build WebSocket message
	msg := models.WSMessage{
		Event:       eventType,
		WorkspaceID: workspaceID,
		Timestamp:   time.Now().UTC(),
		Payload:     payloadJSON,
	}

	// Publish to Redis pub/sub for multi-instance support
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := db.RDB.Publish(ctx, broadcastChannel, msgJSON).Err(); err != nil {
		log.Printf("Warning: Redis publish failed: %v", err)
	}

	// Broadcast to local WebSocket clients
	b.hub.Broadcast(msg)

	// Fan out to in-process SSE subscribers (e.g. GET /events/stream).
	b.deliverToSSE(msg)

	return nil
}

// BroadcastOnly sends a WebSocket event without recording in structure_events.
// Used for high-frequency events like activity logs that have their own table.
func (b *Broadcaster) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("BroadcastOnly: marshal error: %v", err)
		return
	}

	// Boot-telemetry tap (boottrace.go): BOOT_STEP is broadcast-only, so this
	// is the one place its history can be captured for GET /workspaces replay.
	// After the marshal guard — an event that never goes out is not observed.
	b.bootTrace.Observe(eventType, workspaceID, payload)

	msg := models.WSMessage{
		Event:       eventType,
		WorkspaceID: workspaceID,
		Timestamp:   time.Now().UTC(),
		Payload:     payloadJSON,
	}

	b.hub.Broadcast(msg)

	// Fan out to in-process SSE subscribers.
	b.deliverToSSE(msg)
}

// SubscribeSSE registers a per-workspace in-process channel for SSE streaming.
// The caller MUST invoke the returned cancel func when it disconnects so the
// subscription is removed and the channel is not leaked.
func (b *Broadcaster) SubscribeSSE(workspaceID string) (<-chan models.WSMessage, func()) {
	sub := &sseSubscription{
		workspaceID: workspaceID,
		ch:          make(chan models.WSMessage, 64),
	}
	b.ssesMu.Lock()
	b.sses = append(b.sses, sub)
	b.ssesMu.Unlock()

	cancel := func() {
		b.ssesMu.Lock()
		defer b.ssesMu.Unlock()
		for i, s := range b.sses {
			if s == sub {
				b.sses = append(b.sses[:i], b.sses[i+1:]...)
				break
			}
		}
	}
	return sub.ch, cancel
}

// deliverToSSE fans msg out to every in-process SSE subscriber watching the
// same workspace. Non-blocking: if a subscriber's buffer is full the event is
// dropped with a log line (the WebSocket path still delivers it).
func (b *Broadcaster) deliverToSSE(msg models.WSMessage) {
	b.ssesMu.RLock()
	defer b.ssesMu.RUnlock()
	for _, s := range b.sses {
		if s.workspaceID != msg.WorkspaceID {
			continue
		}
		select {
		case s.ch <- msg:
		default:
			log.Printf("SSE: subscriber buffer full for workspace %s, dropping event %s", msg.WorkspaceID, msg.Event)
		}
	}
}

// Subscribe listens to Redis pub/sub and relays events to the WebSocket hub.
func (b *Broadcaster) Subscribe(ctx context.Context) {
	sub := db.RDB.Subscribe(ctx, broadcastChannel)
	ch := sub.Channel(redis.WithChannelHealthCheckInterval(30 * time.Second))

	log.Println("Subscribed to Redis broadcast channel")
	for {
		select {
		case <-ctx.Done():
			sub.Close()
			return
		case redisMsg := <-ch:
			if redisMsg == nil {
				continue
			}
			// In single-instance mode, RecordAndBroadcast already calls hub.Broadcast().
			// This subscriber becomes relevant in multi-instance deployments.
			_ = redisMsg
		}
	}
}
