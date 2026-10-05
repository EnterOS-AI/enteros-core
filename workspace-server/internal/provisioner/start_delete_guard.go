package provisioner

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// The delete-vs-provision race (RC09). Provisioning runs in its own goroutine
// (create, restart, bundle import) and Start can spend minutes in a local image
// build before it reaches ContainerCreate. A DELETE landing in that window found
// no container to stop ("already gone"), removed the config volume, and returned
// 200 — and then the provision carried on: ContainerCreate recreated ws-<id>
// (Docker silently re-creating an EMPTY, unlabelled config volume for it) with
// restart=unless-stopped, and the runtime crash-looped on "Config file not
// found" for months on the CI hosts (~300k restarts each), outliving the
// platform that would have swept it.
//
// Two layers close it:
//
//   - Start registers itself per workspace for its whole run, so a delete can
//     CANCEL it and WAIT for it to unwind before tearing down
//     (CancelInflightStart, called by CascadeDelete after the row is marked
//     'removed').
//   - Start re-checks that the workspace was not removed right before
//     ContainerCreate and right after ContainerStart (SetWorkspaceRemovedCheck),
//     and tears down whatever it made if it was — the backstop for a delete
//     that could not wait long enough, or a provision path that started after
//     the delete marked the row.

// ErrWorkspaceRemoved reports that a Start was abandoned because its workspace
// was deleted while it was in flight. Whatever the Start created has already
// been torn down; callers must NOT mark the workspace failed — it is removed.
var ErrWorkspaceRemoved = errors.New("workspace was removed while provisioning")

type inflightStart struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// SetWorkspaceRemovedCheck wires the "has this workspace been deleted?" probe
// that Start consults right before ContainerCreate and right after
// ContainerStart. Call before the first Start; not synchronized. nil (the
// default) disables the check.
func (p *Provisioner) SetWorkspaceRemovedCheck(fn func(ctx context.Context, workspaceID string) bool) {
	p.removedCheck = fn
}

// workspaceRemoved runs the removed-check on a context detached from ctx: a
// delete CANCELS the provision's ctx, and the check must still get an answer
// exactly then.
func (p *Provisioner) workspaceRemoved(ctx context.Context, workspaceID string) bool {
	if p.removedCheck == nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.removedCheck(checkCtx, workspaceID)
}

// trackStart registers a Start for workspaceID and returns the ctx it must run
// under (cancelled, with cause ErrWorkspaceRemoved, by CancelInflightStart) and
// the func that deregisters it when Start returns.
func (p *Provisioner) trackStart(parent context.Context, workspaceID string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	s := &inflightStart{cancel: cancel, done: make(chan struct{})}
	p.inflightMu.Lock()
	if p.inflight == nil {
		p.inflight = make(map[string]map[*inflightStart]struct{})
	}
	if p.inflight[workspaceID] == nil {
		p.inflight[workspaceID] = make(map[*inflightStart]struct{})
	}
	p.inflight[workspaceID][s] = struct{}{}
	p.inflightMu.Unlock()
	return ctx, func() {
		p.inflightMu.Lock()
		delete(p.inflight[workspaceID], s)
		if len(p.inflight[workspaceID]) == 0 {
			delete(p.inflight, workspaceID)
		}
		p.inflightMu.Unlock()
		close(s.done)
		cancel(nil)
	}
}

// CancelInflightStart cancels every Start currently running for workspaceID
// and waits up to `wait` for them to return. It reports whether none is still
// running. Call it only AFTER the workspace row is marked removed: a cancelled
// Start that unwinds before the row says so would mark the workspace failed.
func (p *Provisioner) CancelInflightStart(workspaceID string, wait time.Duration) bool {
	if p == nil {
		return true
	}
	p.inflightMu.Lock()
	running := make([]*inflightStart, 0, len(p.inflight[workspaceID]))
	for s := range p.inflight[workspaceID] {
		running = append(running, s)
	}
	p.inflightMu.Unlock()
	if len(running) == 0 {
		return true
	}
	for _, s := range running {
		s.cancel(ErrWorkspaceRemoved)
	}
	if wait <= 0 { // cancel only; report without waiting
		for _, s := range running {
			select {
			case <-s.done:
			default:
				return false
			}
		}
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for _, s := range running {
		select {
		case <-s.done:
		case <-timer.C:
			log.Printf("Provisioner: an in-flight start for %s did not unwind within %s of being cancelled for delete — the start's own removed-check will tear down anything it creates", workspaceID, wait)
			return false
		}
	}
	return true
}

// discardRemovedWorkspace tears down what a Start made for a workspace that was
// deleted under it: the ws-<id> container (with its anonymous volumes) and the
// config / claude-sessions volumes, i.e. what CascadeDelete removes. Detached
// from the provision's ctx, which a delete has usually just cancelled.
func (p *Provisioner) discardRemovedWorkspace(ctx context.Context, workspaceID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	name := ContainerName(workspaceID)
	if err := p.cli.ContainerRemove(cleanupCtx, name, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !isContainerNotFound(err) {
		log.Printf("Provisioner: discard of removed workspace %s: remove container %s: %v", workspaceID, name, err)
	}
	if err := p.RemoveVolume(cleanupCtx, workspaceID); err != nil {
		log.Printf("Provisioner: discard of removed workspace %s: %v", workspaceID, err)
	}
	log.Printf("Provisioner: workspace %s was removed while provisioning — discarded its container + volumes", workspaceID)
}

// workspaceRestartPolicy is the Docker restart policy for ws-<id> containers.
// unless-stopped (the default) keeps a workspace up across runtime crashes and
// daemon restarts. MOLECULE_WORKSPACE_RESTART_POLICY overrides it — CI sets
// "no": a workspace that outlives the e2e run that made it must exit once and
// be collectable, not crash-loop on a shared runner host.
func workspaceRestartPolicy() container.RestartPolicy {
	switch v := strings.TrimSpace(os.Getenv("MOLECULE_WORKSPACE_RESTART_POLICY")); v {
	case "":
		return container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	case string(container.RestartPolicyDisabled), string(container.RestartPolicyAlways),
		string(container.RestartPolicyUnlessStopped), string(container.RestartPolicyOnFailure):
		return container.RestartPolicy{Name: container.RestartPolicyMode(v)}
	default:
		log.Printf("Provisioner: ignoring invalid MOLECULE_WORKSPACE_RESTART_POLICY=%q (want no|always|unless-stopped|on-failure); using unless-stopped", v)
		return container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	}
}
