package handlers

// light_monitor.go — the Enter OS Light monitor. One supervised loop in each
// tenant platform with a control plane:
//
//   - every LIGHT_MONITOR_INTERVAL_S (30 s): finish or continue parks and opens
//     that an interruption left half-way, park again a refresh open that did
//     not bring the agent up, resume the schedules an interrupted open left
//     paused (SweepTransitions); park idle open agents (SweepAutoPark); open
//     parked agents that have queued work (SweepRefresh); delete the Light
//     objects of deleted workspaces (SweepRemoved);
//   - every LIGHT_RUN_POLL_INTERVAL_S (300 s), lightRunsPerTick agents a tick
//     until every Light agent was read: record runs that never reported from
//     the control plane's jobs listing, suspend a schedule after three failed
//     runs in a row and tell the owner, revoke a retired run token once no run
//     can hold it (SweepRuns); hourly, prune ledger rows older than 30 days.
//
// Every step on an agent takes its Light lock with TryLock, releases it with
// defer, and skips the agent when an owner call or another step holds it; the
// database transitions are guarded on their own. A sweep acts on at most
// lightSweepMaxSteps agents and starts no new agent's step after
// lightSweepBudget (each control-plane call is bounded by lightBackendTimeout),
// so a slow control plane defers the rest to the next tick instead of stalling
// the loop. Every tick ends with supervised.Heartbeat("light-monitor"), which
// /admin/liveness shows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/supervised"
)

// lightMonitorName is the monitor's name in supervised's liveness registry.
const lightMonitorName = "light-monitor"

const (
	defaultLightMonitorInterval = 30 * time.Second
	defaultLightRunPoll         = 5 * time.Minute
	defaultLightAutoParkIdle    = 30 * time.Minute
	// lightMaxRefreshInFlight caps the agents a refresh has open at once: a
	// global secret change queues a refresh on every parked agent. An agent
	// whose refresh open failed does not hold a slot.
	lightMaxRefreshInFlight = 2
	// A refresh-opened agent parks again once it has gone this long without
	// activity: the turn-start heartbeat of a message the queue drain sent at
	// boot has landed by then (review 21718 R3).
	lightRefreshQuiet = time.Minute
	// A refresh open that has not brought the agent up within this is parked
	// again on its previous snapshot (review 21718 R4).
	lightRefreshOpenTimeout = 15 * time.Minute
	// A park found 'parking' with the pod up this long was interrupted.
	lightParkStuckAfter = 2 * time.Minute
	// A park whose pod has not stopped this long gets its stop re-issued.
	lightStopStuckAfter = 3 * time.Minute
	// A 'hibernating' park this old is finished by hand.
	lightHibernatingStuckAfter = 5 * time.Minute
	// An open marker this old on a parked agent was left by an interrupted
	// open (an open's pause takes at most lightBackendTimeout).
	lightOpenIncompleteAfter = 2 * time.Minute
	// An agent whose last Light step failed this recently is not auto-parked
	// or refreshed again yet.
	lightErrorBackoffSQL = `(light_last_error_at IS NULL OR light_last_error_at < now() - interval '10 minutes')`
	lightRunRetention    = 30 * 24 * time.Hour
	lightPruneEvery      = time.Hour
	// lightSweepLimit bounds the rows one sweep reads.
	lightSweepLimit = 20
	// lightRunsPerTick bounds the agents whose jobs listing one tick reads.
	lightRunsPerTick = 10
	// lightSweepMaxSteps and lightSweepBudget bound one sweep's control-plane
	// work (review 21718 O5): it acts on at most this many agents, and starts
	// no new agent's step once the budget is spent.
	lightSweepMaxSteps = 5
	lightSweepBudget   = 90 * time.Second
)

// lightOpenIncompleteMessage is the open marker's message (GET /light shows it).
const lightOpenIncompleteMessage = "an open paused the schedules and has not woken the agent yet; if it does not finish, the schedules are resumed"

// LightMonitor drives the background half of Enter OS Light.
type LightMonitor struct {
	wh         *WorkspaceHandler
	interval   time.Duration
	runPoll    time.Duration
	idle       time.Duration
	maxRefresh int
	maxSteps   int
	budget     time.Duration

	lastRunPoll time.Time
	// runsCursor is the last agent id a run poll in progress read; "" when no
	// poll is in progress.
	runsCursor string
	lastPrune  time.Time
}

// NewLightMonitor reads LIGHT_MONITOR_INTERVAL_S, LIGHT_RUN_POLL_INTERVAL_S and
// LIGHT_AUTO_PARK_IDLE_MINUTES.
func NewLightMonitor(wh *WorkspaceHandler) *LightMonitor {
	idle := defaultLightAutoParkIdle
	if v := os.Getenv("LIGHT_AUTO_PARK_IDLE_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			idle = time.Duration(n) * time.Minute
		} else {
			log.Printf("LightMonitor: invalid LIGHT_AUTO_PARK_IDLE_MINUTES=%q, using %s", v, idle)
		}
	}
	return &LightMonitor{
		wh:         wh,
		interval:   envDuration("LIGHT_MONITOR_INTERVAL_S", defaultLightMonitorInterval),
		runPoll:    envDuration("LIGHT_RUN_POLL_INTERVAL_S", defaultLightRunPoll),
		idle:       idle,
		maxRefresh: lightMaxRefreshInFlight,
		maxSteps:   lightSweepMaxSteps,
		budget:     lightSweepBudget,
	}
}

// LightMonitorEnabled reports whether main.go starts the monitor: only with a
// control plane (Light runs on Kubernetes tenants), and not when
// LIGHT_MONITOR_DISABLED=true.
func LightMonitorEnabled(hasControlPlane bool) bool {
	return hasControlPlane && !strings.EqualFold(os.Getenv("LIGHT_MONITOR_DISABLED"), "true")
}

// Start ticks until ctx is cancelled; the first tick runs at once.
func (m *LightMonitor) Start(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	log.Printf("LightMonitor: started (interval=%s, run-poll=%s, auto-park idle=%s)", m.interval, m.runPoll, m.idle)
	tick := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("LightMonitor: PANIC in tick — recovered: %v", r)
			}
		}()
		m.Tick(ctx)
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			log.Printf("LightMonitor: stopped")
			return
		case <-t.C:
			tick()
		}
	}
}

// Tick runs one pass of every sweep that is due, then records the monitor as
// alive (a tick that panics or hangs leaves the liveness entry stale).
func (m *LightMonitor) Tick(ctx context.Context) {
	m.SweepTransitions(ctx)
	m.SweepAutoPark(ctx)
	m.SweepRefresh(ctx)
	m.SweepRemoved(ctx)
	now := time.Now()
	if m.runsCursor != "" || now.Sub(m.lastRunPoll) >= m.runPoll {
		if m.runsCursor == "" {
			m.lastRunPoll = now
		}
		m.SweepRuns(ctx)
	}
	if now.Sub(m.lastPrune) >= lightPruneEvery {
		m.lastPrune = now
		if n, err := pruneLightRuns(ctx, lightRunRetention); err != nil {
			log.Printf("LightMonitor: pruning the run ledger failed: %v", err)
		} else if n > 0 {
			log.Printf("LightMonitor: pruned %d run(s) older than %s", n, lightRunRetention)
		}
	}
	supervised.Heartbeat(lightMonitorName)
}

// LightSweepResult counts what one sweep did. Deferred counts the agents left
// for the next tick because the sweep's budget was spent.
type LightSweepResult struct {
	Considered, Acted, Errors, Deferred int
}

// lightSweepGate bounds one sweep's control-plane work: steps left, and the
// time after which no new step starts.
type lightSweepGate struct {
	deadline time.Time
	left     int
}

func (m *LightMonitor) gate(steps int) *lightSweepGate {
	return &lightSweepGate{deadline: time.Now().Add(m.budget), left: steps}
}

// open reports whether another agent's step may start.
func (g *lightSweepGate) open() bool { return g.left > 0 && time.Now().Before(g.deadline) }

// took records one agent's step.
func (g *lightSweepGate) took() { g.left-- }

// queryLightIDs runs a query whose rows start with a workspace id.
func queryLightIDs(ctx context.Context, query string, args ...interface{}) ([][]string, error) {
	rows, err := db.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// lightTransitionRow is one agent SweepTransitions looks at.
type lightTransitionRow struct {
	id       string
	state    string
	status   models.WorkspaceStatus
	age      time.Duration // since light_state_at
	openedBy string
}

// SweepTransitions finishes parks and opens an interruption (a core restart, a
// control-plane outage) left half-way, parks again a refresh open whose agent
// did not come up, and resumes the schedules of a parked agent an interrupted
// open left paused.
func (m *LightMonitor) SweepTransitions(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	rows, err := queryLightIDs(ctx, `
		SELECT id, light_state, status, EXTRACT(EPOCH FROM (now() - COALESCE(light_state_at, now())))::bigint,
		       COALESCE(light_opened_by, '')
		FROM workspaces
		WHERE status != 'removed' AND (
		      light_state IN ('parking', 'opening')
		   OR (light_state = 'open' AND light_opened_by = 'refresh' AND status NOT IN ('online', 'degraded', 'paused'))
		   OR (light_state = 'parked' AND light_last_error->>'code' = $2
		       AND light_last_error_at < now() - make_interval(secs => $3)))
		ORDER BY light_state_at
		LIMIT $1`, lightSweepLimit, lightCodeOpenIncomplete, int(lightOpenIncompleteAfter/time.Second))
	if err != nil {
		log.Printf("LightMonitor: transitions query failed: %v", err)
		res.Errors++
		return res
	}
	gate := m.gate(m.maxSteps)
	for i, r := range rows {
		if !gate.open() {
			res.Deferred += len(rows) - i
			break
		}
		ageSeconds, _ := strconv.ParseInt(r[3], 10, 64)
		row := lightTransitionRow{id: r[0], state: r[1], status: models.WorkspaceStatus(r[2]),
			age: time.Duration(ageSeconds) * time.Second, openedBy: r[4]}
		res.Considered++
		ran, acted, err := m.transitionOne(ctx, row)
		if !ran {
			continue
		}
		if acted || err != nil {
			gate.took()
		}
		if acted {
			res.Acted++
		}
		if err != nil && !errors.Is(err, errLightParkAbandoned) {
			log.Printf("LightMonitor: ws=%s %s/%s: %v", row.id, row.state, row.status, err)
			res.Errors++
		}
	}
	return res
}

// transitionOne runs finishTransition under the agent's Light lock; ran is
// false when another operation holds it.
func (m *LightMonitor) transitionOne(ctx context.Context, row lightTransitionRow) (ran, acted bool, err error) {
	lock := lightOpLock(row.id)
	if !lock.TryLock() {
		return false, false, nil
	}
	defer lock.Unlock()
	acted, err = m.finishTransition(ctx, row)
	return true, acted, err
}

func (m *LightMonitor) finishTransition(ctx context.Context, row lightTransitionRow) (bool, error) {
	h := m.wh
	id, status, age := row.id, row.status, row.age
	awake := status == models.StatusOnline || status == models.StatusDegraded
	switch row.state {
	case models.LightParked:
		// An open paused the CronJobs and never claimed 'opening' (review 21718 O3).
		return true, h.resumeAfterInterruptedOpen(ctx, id)
	case models.LightOpen:
		// A refresh-opened agent whose pod is not up.
		if status == models.StatusFailed || age > lightRefreshOpenTimeout {
			return h.reparkFailedRefreshOpen(ctx, id)
		}
		return false, nil
	case models.LightOpening:
		if row.openedBy == lightOpenedByRefresh && !awake && (status == models.StatusFailed || age > lightRefreshOpenTimeout) {
			return h.reparkFailedRefreshOpen(ctx, id)
		}
		switch status {
		case models.StatusOnline, models.StatusDegraded:
			changed, err := h.lightTransition(ctx, id, "open", lightTriggerMonitor, []string{models.LightOpening}, models.LightOpen)
			if err == nil && changed {
				clearLightError(ctx, id)
			}
			return changed, err
		case models.StatusHibernated:
			// The wake never ran (core restarted first): dispatch it again. Its
			// claim is atomic, so a second dispatch is a no-op.
			h.goAsync(func() { h.WakeWorkspace(id) })
			return true, nil
		case models.StatusFailed:
			recordLightError(ctx, id, "LIGHT_OPEN_FAILED", "the agent failed to start while opening; restart it")
			return h.lightTransition(ctx, id, "open", lightTriggerMonitor, []string{models.LightOpening}, models.LightOpen)
		}
		return false, nil
	}

	// parking
	switch status {
	case models.StatusHibernated:
		err := h.resumeLightAfterPark(ctx, id, lightTriggerMonitor, 0)
		if err != nil && provisioner.LightCPCode(err) == "LIGHT_AGENT_AWAKE" && age > lightStopStuckAfter {
			// Hibernate logs a failed stop and carries on: stop it again.
			log.Printf("LightMonitor: ws=%s still has a running pod %s into its park; stopping it again", id, age)
			if serr := h.StopWorkspaceAuto(ctx, id); serr != nil {
				return true, serr
			}
			return true, nil
		}
		return err == nil, err
	case models.StatusHibernating:
		if age <= lightHibernatingStuckAfter {
			return false, nil
		}
		if err := h.StopWorkspaceAuto(ctx, id); err != nil {
			return true, err
		}
		if _, err := db.DB.ExecContext(ctx, `
			UPDATE workspaces SET status = $2, url = '', active_tasks = 0, updated_at = now()
			WHERE id = $1 AND status = 'hibernating'`, id, models.StatusHibernated); err != nil {
			return true, err
		}
		db.ClearWorkspaceKeys(ctx, id)
		return true, h.resumeLightAfterPark(ctx, id, lightTriggerMonitor, 0)
	case models.StatusOnline, models.StatusDegraded:
		if age <= lightParkStuckAfter {
			return false, nil
		}
		// parkLight continues the park with the trigger and the refresh
		// watermark its claim recorded.
		res, ref := h.parkLight(ctx, nil, id, lightTriggerMonitor)
		if ref != nil {
			return true, fmt.Errorf("continuing the park: %d %s", ref.Status, ref.code())
		}
		if res.Unchanged {
			return false, nil
		}
		return true, h.finishLightPark(ctx, id, lightTriggerMonitor, 0)
	case models.StatusFailed:
		// The pod failed before it stopped; its CronJobs are still paused.
		recordLightError(ctx, id, "LIGHT_PARK_FAILED", "the agent failed while it was being parked; it is open again")
		return h.lightTransition(ctx, id, "park", lightTriggerMonitor, []string{models.LightParking}, models.LightOpen)
	}
	return false, nil
}

// reparkFailedRefreshOpen parks again, on its previous snapshot, an agent a
// refresh opened that did not come up (its pod failed, or it was not online
// within lightRefreshOpenTimeout), so it stops holding a refresh slot (review
// 21718 R4). Its CronJobs, Secret and run token are still those of its last
// park, so stopping the pod and resuming the CronJobs is a complete park; its
// queued refreshes stay queued and are retried once the error back-off ends
// (LIGHT_REFRESH_OPEN_FAILED survives the park). A pod that keeps running is
// stopped again by the 'parking' sweep (the resume answers LIGHT_AGENT_AWAKE).
func (h *WorkspaceHandler) reparkFailedRefreshOpen(ctx context.Context, workspaceID string) (bool, error) {
	res, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = 'parking', light_state_at = now(), light_park_trigger = 'refresh', light_park_watermark = 0,
		    status = 'hibernated', url = '', active_tasks = 0, updated_at = now(),
		    light_last_error = jsonb_build_object('code', $2::text, 'message', $3::text), light_last_error_at = now()
		WHERE id = $1 AND light_state IN ('opening', 'open') AND light_opened_by = 'refresh'
		  AND status NOT IN ('online', 'degraded', 'paused', 'removed')`,
		workspaceID, lightCodeRefreshOpenFailed,
		"a refresh open did not bring the agent up; it was parked again on its previous configuration and the refresh is retried in 10 minutes")
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // it came up meanwhile, or the owner took it over
	}
	log.Printf("light op=park ws=%s from=[opening open] to=\"parking\" trigger=refresh result=ok reason=refresh-open-failed", workspaceID)
	h.broadcastLightState(workspaceID, models.LightParking)
	db.ClearWorkspaceKeys(ctx, workspaceID)
	if err := h.StopWorkspaceAuto(ctx, workspaceID); err != nil {
		return true, err
	}
	return true, h.resumeLightAfterPark(ctx, workspaceID, lightTriggerRefresh, 0)
}

// resumeAfterInterruptedOpen resumes the CronJobs an open paused before it
// claimed the agent, and clears the open marker. When the resume fails the
// marker is stamped again, so the monitor retries after
// lightOpenIncompleteAfter.
func (h *WorkspaceHandler) resumeAfterInterruptedOpen(ctx context.Context, workspaceID string) error {
	if h.lightAPI == nil {
		return provisioner.ErrLightNotConfigured
	}
	rctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	_, err := h.lightAPI.ResumeLight(rctx, workspaceID)
	cancel()
	if err != nil {
		if _, serr := db.DB.ExecContext(ctx, `
			UPDATE workspaces SET light_last_error_at = now()
			WHERE id = $1 AND light_state = 'parked' AND light_last_error->>'code' = $2`,
			workspaceID, lightCodeOpenIncomplete); serr != nil {
			log.Printf("light: re-stamping the open marker of %s failed: %v", workspaceID, serr)
		}
		return fmt.Errorf("resuming the schedules an interrupted open paused: %w", err)
	}
	log.Printf("light op=open ws=%s: resumed the schedules an interrupted open paused", workspaceID)
	_, err = db.DB.ExecContext(ctx, `
		UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL
		WHERE id = $1 AND light_state = 'parked' AND light_last_error->>'code' = $2`,
		workspaceID, lightCodeOpenIncomplete)
	return err
}

// lightQuietWindowSQL is how long an open agent must have been open, and
// without activity of its own, before it auto-parks: the idle window ($1,
// minutes), or lightRefreshQuiet ($3, seconds) when a refresh opened it.
const lightQuietWindowSQL = `CASE WHEN workspaces.light_opened_by = 'refresh' THEN make_interval(secs => $3) ELSE make_interval(mins => $1) END`

// lightAutoParkCandidates lists open Light agents to park now: online, idle
// (no task, not busy), no Light failure in the last 10 minutes, and neither
// opened nor active within the quiet window. Only the agent's own activity
// counts: rows its runs write carry a run_id (review 21719 O-A). Agents without
// a recorded failure come first. Rows are (id, opened_by).
func lightAutoParkCandidates(ctx context.Context, idle time.Duration) ([][]string, error) {
	return queryLightIDs(ctx, `
		SELECT id, COALESCE(light_opened_by, 'owner')
		FROM workspaces
		WHERE light_state = 'open' AND status IN ('online', 'degraded')
		  AND COALESCE(active_tasks, 0) = 0 AND NOT COALESCE(is_busy, false)
		  AND `+lightErrorBackoffSQL+`
		  AND light_state_at < now() - `+lightQuietWindowSQL+`
		  AND NOT EXISTS (SELECT 1 FROM activity_logs a
		                  WHERE a.workspace_id = workspaces.id AND a.run_id IS NULL
		                    AND a.created_at >= now() - `+lightQuietWindowSQL+`)
		ORDER BY light_last_error_at NULLS FIRST, light_state_at
		LIMIT $2`, int(idle/time.Minute), lightSweepLimit, int(lightRefreshQuiet/time.Second))
}

// SweepAutoPark parks an open agent that has been idle for the auto-park
// window, or quiet for a minute when a refresh opened it (after applying its
// queued work). A refused park records its error, so the error back-off keeps
// the agent out of the next sweeps (review 21718 O2). The park's pod stop and
// resume run in the background under the agent's lock.
func (m *LightMonitor) SweepAutoPark(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	rows, err := lightAutoParkCandidates(ctx, m.idle)
	if err != nil {
		log.Printf("LightMonitor: auto-park query failed: %v", err)
		res.Errors++
		return res
	}
	gate := m.gate(m.maxSteps)
	for i, r := range rows {
		if !gate.open() {
			res.Deferred += len(rows) - i
			break
		}
		id := r[0]
		trig := lightTriggerAuto
		if r[1] == lightOpenedByRefresh {
			trig = lightTriggerRefresh
		}
		res.Considered++
		ran, acted, err := m.autoParkOne(ctx, id, trig)
		if !ran {
			continue
		}
		gate.took()
		if err != nil {
			log.Printf("LightMonitor: ws=%s %v", id, err)
			res.Errors++
			continue
		}
		if acted {
			res.Acted++
		}
	}
	return res
}

// autoParkOne parks one agent under its Light lock, which the background
// finish takes over when the park went ahead.
func (m *LightMonitor) autoParkOne(ctx context.Context, id string, trig lightTrigger) (ran, acted bool, err error) {
	lock := lightOpLock(id)
	if !lock.TryLock() {
		return false, false, nil
	}
	handedOff := false
	defer func() {
		if !handedOff {
			lock.Unlock()
		}
	}()
	if trig == lightTriggerRefresh {
		m.wh.applyLightPendingActions(ctx, id)
	}
	park, ref := m.wh.parkLight(ctx, nil, id, trig)
	if ref != nil {
		recordLightError(ctx, id, ref.code(), fmt.Sprint(ref.Body["error"]))
		return true, false, fmt.Errorf("%s park refused: %d %s", trig, ref.Status, ref.code())
	}
	if park.Unchanged {
		return true, false, nil
	}
	handedOff = true
	m.wh.goAsync(func() {
		defer lock.Unlock()
		fctx, cancel := context.WithTimeout(context.Background(), lightTransitionTimeout)
		defer cancel()
		if err := m.wh.finishLightPark(fctx, id, trig, lightResumeInlineBudget); err != nil && !errors.Is(err, errLightParkAbandoned) {
			log.Printf("LightMonitor: ws=%s finishing the %s park: %v (retried)", id, trig, err)
		}
	})
	return true, true, nil
}

// lightActionAppliers apply one queued action to an open, idle agent before a
// refresh cycle parks it again. A kind with no entry here is left queued (a
// newer core may know it). 'refresh' needs nothing applied: the park's env
// snapshot is what it waits for (markLightRefreshApplied).
var lightActionAppliers = map[string]func(ctx context.Context, h *WorkspaceHandler, workspaceID string, payload json.RawMessage) error{
	lightActionRefresh: nil,
}

// lightKnownActionKinds lists the kinds this core can complete.
func lightKnownActionKinds() []string {
	kinds := make([]string, 0, len(lightActionAppliers))
	for k := range lightActionAppliers {
		kinds = append(kinds, k)
	}
	return kinds
}

// applyLightPendingActions applies the agent's queued actions that have an
// applier, oldest first, recording each outcome on its row. A failure is also
// recorded as the agent's Light error, which the park this cycle ends with
// keeps: the error back-off then spaces the retries (review 21718 N7).
func (h *WorkspaceHandler) applyLightPendingActions(ctx context.Context, workspaceID string) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, kind, payload FROM light_pending_actions
		WHERE workspace_id = $1 AND applied_at IS NULL AND kind <> $2
		ORDER BY id`, workspaceID, lightActionRefresh)
	if err != nil {
		log.Printf("light: reading the queued actions of %s failed: %v", workspaceID, err)
		return
	}
	type action struct {
		id      int64
		kind    string
		payload json.RawMessage
	}
	var actions []action
	for rows.Next() {
		var a action
		var payload []byte
		if rows.Scan(&a.id, &a.kind, &payload) == nil {
			a.payload = payload
			actions = append(actions, a)
		}
	}
	rows.Close()
	for _, a := range actions {
		apply := lightActionAppliers[a.kind]
		if apply == nil {
			continue
		}
		if err := apply(ctx, h, workspaceID, a.payload); err != nil {
			log.Printf("light: applying %s action %d of %s failed: %v", a.kind, a.id, workspaceID, err)
			msg := headUTF8(lightStorableText(err.Error()), lightErrorLimit)
			_, _ = db.DB.ExecContext(ctx, `UPDATE light_pending_actions SET error = $2 WHERE id = $1`, a.id, msg)
			recordLightError(ctx, workspaceID, lightCodeActionFailed,
				headUTF8(fmt.Sprintf("applying a queued %s action failed: %s", a.kind, msg), 1000))
			continue
		}
		_, _ = db.DB.ExecContext(ctx, `UPDATE light_pending_actions SET applied_at = now(), error = NULL WHERE id = $1`, a.id)
	}
}

// lightRefreshCandidates lists parked agents with queued work of the given
// kinds, oldest queued first, without a Light failure in the last 10 minutes.
func lightRefreshCandidates(ctx context.Context, kinds []string, limit int) ([][]string, error) {
	return queryLightIDs(ctx, `
		SELECT w.id FROM workspaces w
		JOIN LATERAL (
			SELECT MIN(a.id) AS first_id FROM light_pending_actions a
			WHERE a.workspace_id = w.id AND a.applied_at IS NULL AND a.kind = ANY($1)
		) p ON p.first_id IS NOT NULL
		WHERE w.light_state = 'parked' AND w.status = 'hibernated'
		  AND (w.light_last_error_at IS NULL OR w.light_last_error_at < now() - interval '10 minutes')
		ORDER BY p.first_id
		LIMIT $2`, pq.Array(kinds), limit)
}

// lightRefreshInFlightSQL counts the refresh cycles holding a slot: agents a
// refresh opened that are opening or open, except those whose pod failed
// (SweepTransitions parks them again) and those the owner paused (they stay
// as the owner left them; review 21719 N-B).
const lightRefreshInFlightSQL = `
		SELECT COUNT(*) FROM workspaces
		WHERE light_state IN ('opening', 'open') AND light_opened_by = 'refresh'
		  AND status NOT IN ('failed', 'paused', 'removed')`

// SweepRefresh opens parked agents that have queued work this core can
// complete, keeping at most maxRefresh refresh cycles in flight. Each opened
// agent parks again once it is online and quiet (SweepAutoPark).
func (m *LightMonitor) SweepRefresh(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	var inFlight int
	if err := db.DB.QueryRowContext(ctx, lightRefreshInFlightSQL).Scan(&inFlight); err != nil {
		log.Printf("LightMonitor: refresh count failed: %v", err)
		res.Errors++
		return res
	}
	budget := m.maxRefresh - inFlight
	if budget <= 0 {
		return res
	}
	rows, err := lightRefreshCandidates(ctx, lightKnownActionKinds(), budget)
	if err != nil {
		log.Printf("LightMonitor: refresh query failed: %v", err)
		res.Errors++
		return res
	}
	gate := m.gate(budget)
	for i, r := range rows {
		if !gate.open() {
			res.Deferred += len(rows) - i
			break
		}
		id := r[0]
		res.Considered++
		ran, err := m.refreshOne(ctx, id)
		if !ran {
			continue
		}
		gate.took()
		if err != nil {
			log.Printf("LightMonitor: ws=%s %v", id, err)
			res.Errors++
			continue
		}
		res.Acted++
	}
	return res
}

// refreshOne opens one parked agent for a refresh under its Light lock. A
// refusal is recorded (the refresh back-off), except over an open marker,
// which the transitions sweep needs to resume the paused schedules.
func (m *LightMonitor) refreshOne(ctx context.Context, id string) (ran bool, err error) {
	lock := lightOpLock(id)
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	_, _, _, ref := m.wh.openLight(ctx, id, lightTriggerRefresh)
	if ref != nil {
		recordLightErrorKeepingOpenMarker(ctx, id, ref.code(), fmt.Sprint(ref.Body["error"]))
		return true, fmt.Errorf("refresh open refused: %d %s", ref.Status, ref.code())
	}
	return true, nil
}

// SweepRemoved deletes the Light objects a workspace delete could not (the
// row keeps light_state as the marker) and clears its Light columns.
func (m *LightMonitor) SweepRemoved(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	rows, err := queryLightIDs(ctx, `
		SELECT id FROM workspaces WHERE status = 'removed' AND light_state IS NOT NULL LIMIT $1`, lightSweepLimit)
	if err != nil {
		log.Printf("LightMonitor: removed query failed: %v", err)
		res.Errors++
		return res
	}
	gate := m.gate(m.maxSteps)
	for i, r := range rows {
		if !gate.open() {
			res.Deferred += len(rows) - i
			break
		}
		id := r[0]
		res.Considered++
		ran, err := m.removedOne(ctx, id)
		if !ran {
			continue
		}
		gate.took()
		if err != nil {
			log.Printf("LightMonitor: ws=%s deleting the Light objects of a removed workspace: %v (retried)", id, err)
			res.Errors++
			continue
		}
		res.Acted++
	}
	return res
}

// removedOne deletes one removed workspace's Light objects under its lock. A
// failed delete writes nothing: the marker stays for the next sweep.
func (m *LightMonitor) removedOne(ctx context.Context, id string) (ran bool, err error) {
	lock := lightOpLock(id)
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	if _, err := m.wh.deleteLightObjects(ctx, id); err != nil {
		return true, err
	}
	clearLightColumnsAfterDelete(ctx, id)
	return true, nil
}

// lightJobStatus maps a finished Job to a ledger status.
func lightJobStatus(j provisioner.LightJob) string {
	if j.Phase == "succeeded" {
		return models.LightRunSucceeded
	}
	if j.Reason == "DeadlineExceeded" || (j.ExitCode != nil && *j.ExitCode == 124) {
		return models.LightRunTimedOut
	}
	return models.LightRunFailed
}

// lightJobError is the ledger error of a Job core recorded itself.
func lightJobError(j provisioner.LightJob) string {
	if j.Phase == "succeeded" {
		return ""
	}
	reason := j.Reason
	if reason == "" {
		reason = "the run's pod failed"
	}
	if j.ExitCode != nil {
		return fmt.Sprintf("%s (exit %d)", reason, *j.ExitCode)
	}
	return reason
}

func parseLightJobTime(s *string) interface{} {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return t.UTC()
}

// recordLightJobs writes what the jobs listing says about runs: a running Job
// with no row gets a running row; a finished Job closes a running row or adds
// a row for a run that never reported. A reported outcome is never
// overwritten. Returns the schedules that saw a failed or timed-out run, and
// whether any run (whatever its names) is unfinished.
func recordLightJobs(ctx context.Context, workspaceID string, jobs []provisioner.LightJob) (failedSchedules []string, unfinished bool) {
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.Phase == "pending" || j.Phase == "running" {
			// Before the name checks: a run with a name this core cannot store
			// may still hold the retired run token (review 21718 N2).
			unfinished = true
		}
		if !lightRunIDRe.MatchString(j.RunID) || !validLightScheduleName(j.Schedule) {
			continue
		}
		scheduledFor := parseLightJobTime(j.ScheduledFor)
		started := parseLightJobTime(j.StartedAt)
		switch j.Phase {
		case "running":
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, scheduled_for, started_at)
				VALUES ($1, $2, $3, 'running', 'job', $4, $5)
				ON CONFLICT (workspace_id, run_id) DO UPDATE
					SET scheduled_for = COALESCE(light_runs.scheduled_for, EXCLUDED.scheduled_for)`,
				workspaceID, j.RunID, j.Schedule, scheduledFor, started); err != nil {
				log.Printf("light: recording running job %s of %s failed: %v", j.RunID, workspaceID, err)
			}
		case "succeeded", "failed":
			status := lightJobStatus(j)
			finished := parseLightJobTime(j.FinishedAt)
			if finished == nil {
				finished = time.Now().UTC()
			}
			var exit interface{}
			if j.ExitCode != nil {
				exit = int(*j.ExitCode)
			}
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, error)
				VALUES ($1, $2, $3, $4, 'job', $5, $6, $7, $8, $9)
				ON CONFLICT (workspace_id, run_id) DO UPDATE
					SET status = EXCLUDED.status, source = 'job', finished_at = EXCLUDED.finished_at,
					    exit_code = EXCLUDED.exit_code, error = EXCLUDED.error,
					    scheduled_for = COALESCE(light_runs.scheduled_for, EXCLUDED.scheduled_for), updated_at = now()
					WHERE light_runs.status = 'running'`,
				workspaceID, j.RunID, j.Schedule, status, scheduledFor, started, finished, exit, headUTF8(lightStorableText(lightJobError(j)), lightErrorLimit)); err != nil {
				log.Printf("light: recording finished job %s of %s failed: %v", j.RunID, workspaceID, err)
				continue
			}
			if scheduledFor != nil {
				_, _ = db.DB.ExecContext(ctx, `
					UPDATE light_runs SET scheduled_for = $3
					WHERE workspace_id = $1 AND run_id = $2 AND scheduled_for IS NULL`, workspaceID, j.RunID, scheduledFor)
			}
			if status != models.LightRunSucceeded && !seen[j.Schedule] {
				seen[j.Schedule] = true
				failedSchedules = append(failedSchedules, j.Schedule)
			}
		}
	}
	return failedSchedules, unfinished
}

// lightRunsZeroCursor is the cursor a run poll starts from.
const lightRunsZeroCursor = "00000000-0000-0000-0000-000000000000"

// SweepRuns reads the jobs listing of the next lightRunsPerTick Light agents
// (by id, carrying on from where the last tick stopped): runs that never
// reported are recorded from their Jobs, failure streaks are evaluated, and a
// retired run token is revoked once no run is unfinished.
func (m *LightMonitor) SweepRuns(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	if m.wh.lightAPI == nil {
		return res
	}
	after := m.runsCursor
	if after == "" {
		after = lightRunsZeroCursor
	}
	rows, err := queryLightIDs(ctx, `
		SELECT w.id, COALESCE(w.light_retired_token_id::text, '') FROM workspaces w
		WHERE w.light_state IS NOT NULL AND w.status != 'removed' AND w.id > $1::uuid
		  AND EXISTS (SELECT 1 FROM light_schedules s WHERE s.workspace_id = w.id)
		ORDER BY w.id
		LIMIT $2`, after, lightRunsPerTick)
	if err != nil {
		log.Printf("LightMonitor: runs query failed: %v", err)
		res.Errors++
		return res
	}
	// A full page means more agents may follow: the next tick carries on.
	m.runsCursor = ""
	if len(rows) == lightRunsPerTick {
		m.runsCursor = rows[len(rows)-1][0]
	}
	gate := m.gate(lightRunsPerTick)
	for i, r := range rows {
		if !gate.open() {
			res.Deferred += len(rows) - i
			m.runsCursor = after
			if i > 0 {
				m.runsCursor = rows[i-1][0]
			}
			break
		}
		id, retired := r[0], r[1]
		res.Considered++
		gate.took()
		acted, errs := m.runsOne(ctx, id, retired)
		if acted {
			res.Acted++
		}
		res.Errors += errs
	}
	return res
}

// runsOne reads one agent's jobs listing and records it; its failure streaks
// and its retired token are handled under its Light lock, so a suspension can
// never land between a park's render and its PUT (review 21718 O1). When a
// transition holds the lock they wait for the next poll (a failed Job stays
// listed for a day).
func (m *LightMonitor) runsOne(ctx context.Context, id, retired string) (acted bool, errs int) {
	jctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	jobs, err := m.wh.lightAPI.ListLightJobs(jctx, id)
	cancel()
	if err != nil {
		log.Printf("LightMonitor: ws=%s jobs listing failed: %v", id, err)
		return false, 1
	}
	failed, unfinished := recordLightJobs(ctx, id, jobs)
	lock := lightOpLock(id)
	if !lock.TryLock() {
		log.Printf("LightMonitor: ws=%s is in a Light transition; its streaks and retired token wait for the next poll", id)
		return false, 0
	}
	defer lock.Unlock()
	for _, schedule := range failed {
		if _, err := m.wh.evaluateLightStreak(ctx, id, schedule); err != nil {
			log.Printf("LightMonitor: ws=%s schedule %q streak: %v", id, schedule, err)
			errs++
		}
	}
	if retired != "" && !unfinished && revokeRetiredLightToken(ctx, id, retired) {
		acted = true
	}
	return acted, errs
}

// revokeRetiredLightToken revokes the previous park's run token once no run
// can hold it, and forgets it.
func revokeRetiredLightToken(ctx context.Context, workspaceID, tokenID string) bool {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET light_retired_token_id = NULL
		WHERE id = $1 AND light_retired_token_id = $2`, workspaceID, tokenID)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_auth_tokens SET revoked_at = now()
		WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL`, workspaceID, tokenID); err != nil {
		return false
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	log.Printf("light: revoked the retired run token of %s (no run holds it)", workspaceID)
	return true
}

// lightLeadingFailures counts the failed or timed-out runs at the head of a
// newest-first list of terminal statuses.
func lightLeadingFailures(statuses []string) int {
	n := 0
	for _, s := range statuses {
		if s != models.LightRunFailed && s != models.LightRunTimedOut {
			break
		}
		n++
	}
	return n
}

// lightStreakLength is how many failed runs in a row suspend a schedule.
const lightStreakLength = 3

// errLightLockHeld: another Light operation holds the agent's lock.
var errLightLockHeld = errors.New("light: another Light operation holds the agent")

// evaluateLightStreakLocked is evaluateLightStreak under the agent's Light
// lock, or errLightLockHeld when a transition holds it (the jobs poll
// evaluates it later): a suspension must never land between a park's render
// and its PUT, which would re-enable the CronJob behind core's back (review
// 21718 O1).
func (h *WorkspaceHandler) evaluateLightStreakLocked(ctx context.Context, workspaceID, schedule string) (bool, error) {
	lock := lightOpLock(workspaceID)
	if !lock.TryLock() {
		return false, errLightLockHeld
	}
	defer lock.Unlock()
	return h.evaluateLightStreak(ctx, workspaceID, schedule)
}

// evaluateLightStreak suspends a schedule whose last three terminal runs
// (cancelled ones excluded; only runs since its last suspension) all failed:
// the control plane suspends its CronJob first; then core claims the
// suspension once and tells the owner. Nothing is written when the control
// plane refuses, so the next evaluation retries. The caller holds the agent's
// Light lock (evaluateLightStreakLocked, runsOne).
func (h *WorkspaceHandler) evaluateLightStreak(ctx context.Context, workspaceID, schedule string) (bool, error) {
	var reason sql.NullString
	var lastSuspended sql.NullTime
	err := db.DB.QueryRowContext(ctx, `
		SELECT suspended_reason, suspended_at FROM light_schedules WHERE workspace_id = $1 AND name = $2`,
		workspaceID, schedule).Scan(&reason, &lastSuspended)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && reason.Valid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var since interface{}
	if lastSuspended.Valid {
		since = lastSuspended.Time
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT status, error FROM light_runs
		WHERE workspace_id = $1 AND schedule = $2 AND status IN ('succeeded', 'failed', 'timed_out')
		  AND ($3::timestamptz IS NULL OR created_at > $3)
		ORDER BY COALESCE(scheduled_for, started_at, created_at) DESC
		LIMIT 3`, workspaceID, schedule, since)
	if err != nil {
		return false, err
	}
	var statuses []string
	lastError := ""
	for rows.Next() {
		var st, e string
		if err := rows.Scan(&st, &e); err != nil {
			rows.Close()
			return false, err
		}
		if len(statuses) == 0 {
			lastError = e
		}
		statuses = append(statuses, st)
	}
	rows.Close()
	n := lightLeadingFailures(statuses)
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE light_schedules SET consecutive_failures = $3
		WHERE workspace_id = $1 AND name = $2 AND consecutive_failures <> $3`, workspaceID, schedule, n); err != nil {
		log.Printf("light: updating the failure count of %s/%q failed: %v", workspaceID, schedule, err)
	}
	if n < lightStreakLength || h.lightAPI == nil {
		return false, nil
	}
	sctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	_, err = h.lightAPI.SuspendLightSchedule(sctx, workspaceID, schedule)
	cancel()
	if err != nil {
		return false, err
	}
	why := fmt.Sprintf("%d runs in a row failed", lightStreakLength)
	var claimed string
	err = db.DB.QueryRowContext(ctx, `
		UPDATE light_schedules SET suspended_reason = $3, suspended_at = now(), updated_at = now()
		WHERE workspace_id = $1 AND name = $2 AND suspended_reason IS NULL
		RETURNING name`, workspaceID, schedule, why).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // another evaluation claimed it
	}
	if err != nil {
		return false, err
	}
	log.Printf("light op=suspend ws=%s schedule=%q reason=%q", workspaceID, schedule, why)
	h.notifyLightScheduleSuspended(ctx, workspaceID, schedule, lastError)
	return true, nil
}

// notifyLightScheduleSuspended tells the owner, in the agent's chat, that a
// schedule was suspended and how to resume it. When the agent may not talk to
// the user, the canvas still gets a WORKSPACE_UPDATED naming the schedule
// (GET /light shows it).
func (h *WorkspaceHandler) notifyLightScheduleSuspended(ctx context.Context, workspaceID, schedule, lastError string) {
	last := strings.TrimSpace(lastError)
	if last == "" {
		last = "none recorded"
	}
	last = headUTF8(last, 300)
	msg := fmt.Sprintf("Light: the schedule %q failed %d runs in a row, so it is suspended. Last error: %s. "+
		"To resume it, open the agent, fix the cause (or change the schedule), then park the agent from its Light settings: "+
		"your park resumes every suspended schedule.",
		schedule, lightStreakLength, last)
	err := NewAgentMessageWriter(db.DB, h.broadcaster).Send(ctx, workspaceID, msg, nil)
	if err == nil {
		return
	}
	if !errors.Is(err, ErrTalkToUserDisabled) {
		log.Printf("light: telling the owner of %s about suspended schedule %q failed: %v", workspaceID, schedule, err)
	}
	if h.broadcaster != nil {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventWorkspaceUpdated), map[string]interface{}{
			"light_schedule_suspended": schedule,
		})
	}
}

// pruneLightRuns deletes finished ledger rows older than olderThan.
func pruneLightRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := db.DB.ExecContext(ctx, `
		DELETE FROM light_runs WHERE status <> 'running' AND created_at < now() - make_interval(secs => $1)`,
		int64(olderThan/time.Second))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
