package handlers

// light_monitor.go — the Enter OS Light monitor. One supervised loop in each
// tenant platform with a control plane:
//
//   - every LIGHT_MONITOR_INTERVAL_S (30 s): finish or continue parks and opens
//     that an interruption left half-way (SweepTransitions), park idle open
//     agents (SweepAutoPark), open parked agents that have queued work
//     (SweepRefresh), delete the Light objects of deleted workspaces
//     (SweepRemoved);
//   - every LIGHT_RUN_POLL_INTERVAL_S (300 s): record runs that never reported
//     from the control plane's jobs listing, suspend a schedule after three
//     failed runs in a row and tell the owner, revoke a retired run token once
//     no run can hold it (SweepRuns); hourly, prune ledger rows older than 30
//     days.
//
// Every step on an agent takes its Light lock with TryLock and skips the agent
// when an owner call or another step holds it; the database transitions are
// guarded on their own.

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
)

const (
	defaultLightMonitorInterval = 30 * time.Second
	defaultLightRunPoll         = 5 * time.Minute
	defaultLightAutoParkIdle    = 30 * time.Minute
	// lightMaxRefreshInFlight caps the agents a refresh has open at once: a
	// global secret change queues a refresh on every parked agent.
	lightMaxRefreshInFlight = 2
	// A park found 'parking' with the pod up this long was interrupted.
	lightParkStuckAfter = 2 * time.Minute
	// A park whose pod has not stopped this long gets its stop re-issued.
	lightStopStuckAfter = 3 * time.Minute
	// A 'hibernating' park this old is finished by hand.
	lightHibernatingStuckAfter = 5 * time.Minute
	// An agent whose last Light step failed this recently is not auto-parked
	// or refreshed again yet.
	lightErrorBackoffSQL = `(light_last_error_at IS NULL OR light_last_error_at < now() - interval '10 minutes')`
	lightRunRetention    = 30 * 24 * time.Hour
	lightPruneEvery      = time.Hour
	lightSweepLimit      = 20
)

// LightMonitor drives the background half of Enter OS Light.
type LightMonitor struct {
	wh         *WorkspaceHandler
	interval   time.Duration
	runPoll    time.Duration
	idle       time.Duration
	maxRefresh int

	lastRunPoll time.Time
	lastPrune   time.Time
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

// Tick runs one pass of every sweep that is due.
func (m *LightMonitor) Tick(ctx context.Context) {
	m.SweepTransitions(ctx)
	m.SweepAutoPark(ctx)
	m.SweepRefresh(ctx)
	m.SweepRemoved(ctx)
	now := time.Now()
	if now.Sub(m.lastRunPoll) >= m.runPoll {
		m.lastRunPoll = now
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
}

// LightSweepResult counts what one sweep did.
type LightSweepResult struct {
	Considered, Acted, Errors int
}

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

// SweepTransitions finishes parks and opens an interruption (a core restart, a
// control-plane outage) left half-way.
func (m *LightMonitor) SweepTransitions(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	rows, err := queryLightIDs(ctx, `
		SELECT id, light_state, status, EXTRACT(EPOCH FROM (now() - COALESCE(light_state_at, now())))::bigint
		FROM workspaces
		WHERE light_state IN ('parking', 'opening') AND status != 'removed'
		ORDER BY light_state_at
		LIMIT $1`, lightSweepLimit)
	if err != nil {
		log.Printf("LightMonitor: transitions query failed: %v", err)
		res.Errors++
		return res
	}
	for _, r := range rows {
		id, state, status := r[0], r[1], models.WorkspaceStatus(r[2])
		ageSeconds, _ := strconv.ParseInt(r[3], 10, 64)
		age := time.Duration(ageSeconds) * time.Second
		res.Considered++
		lock := lightOpLock(id)
		if !lock.TryLock() {
			continue
		}
		acted, err := m.finishTransition(ctx, id, state, status, age)
		lock.Unlock()
		if acted {
			res.Acted++
		}
		if err != nil && !errors.Is(err, errLightParkAbandoned) {
			log.Printf("LightMonitor: ws=%s %s/%s: %v", id, state, status, err)
			res.Errors++
		}
	}
	return res
}

func (m *LightMonitor) finishTransition(ctx context.Context, id, state string, status models.WorkspaceStatus, age time.Duration) (bool, error) {
	h := m.wh
	if state == models.LightOpening {
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

// lightAutoParkCandidates lists open Light agents to park now: online, idle
// (no task, not busy), no Light failure in the last 10 minutes, and either
// opened by a refresh or without activity for the idle window. Rows are
// (id, opened_by).
func lightAutoParkCandidates(ctx context.Context, idle time.Duration) ([][]string, error) {
	return queryLightIDs(ctx, `
		SELECT id, COALESCE(light_opened_by, 'owner')
		FROM workspaces
		WHERE light_state = 'open' AND status IN ('online', 'degraded')
		  AND COALESCE(active_tasks, 0) = 0 AND NOT COALESCE(is_busy, false)
		  AND `+lightErrorBackoffSQL+`
		  AND (light_opened_by = 'refresh'
		       OR GREATEST(COALESCE(last_activity_at, light_state_at), light_state_at) < now() - make_interval(mins => $1))
		ORDER BY light_state_at
		LIMIT $2`, int(idle/time.Minute), lightSweepLimit)
}

// SweepAutoPark parks an open agent that has been idle for the auto-park
// window, or at once when a refresh opened it (after applying its queued
// work). The park's pod stop and resume run in the background under the
// agent's lock.
func (m *LightMonitor) SweepAutoPark(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	rows, err := lightAutoParkCandidates(ctx, m.idle)
	if err != nil {
		log.Printf("LightMonitor: auto-park query failed: %v", err)
		res.Errors++
		return res
	}
	for _, r := range rows {
		id := r[0]
		trig := lightTriggerAuto
		if r[1] == lightOpenedByRefresh {
			trig = lightTriggerRefresh
		}
		res.Considered++
		lock := lightOpLock(id)
		if !lock.TryLock() {
			continue
		}
		if trig == lightTriggerRefresh {
			m.wh.applyLightPendingActions(ctx, id)
		}
		park, ref := m.wh.parkLight(ctx, nil, id, trig)
		if ref != nil || park.Unchanged {
			lock.Unlock()
			if ref != nil {
				log.Printf("LightMonitor: ws=%s %s park refused: %d %s", id, trig, ref.Status, ref.code())
				res.Errors++
			}
			continue
		}
		res.Acted++
		m.wh.goAsync(func() {
			defer lock.Unlock()
			fctx, cancel := context.WithTimeout(context.Background(), lightTransitionTimeout)
			defer cancel()
			if err := m.wh.finishLightPark(fctx, id, trig, lightResumeInlineBudget); err != nil && !errors.Is(err, errLightParkAbandoned) {
				log.Printf("LightMonitor: ws=%s finishing the %s park: %v (retried)", id, trig, err)
			}
		})
	}
	return res
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
// applier, oldest first, recording each outcome on its row.
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
			_, _ = db.DB.ExecContext(ctx, `UPDATE light_pending_actions SET error = $2 WHERE id = $1`, a.id, err.Error())
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

// SweepRefresh opens parked agents that have queued work this core can
// complete, keeping at most maxRefresh refresh cycles in flight. Each opened
// agent parks again as soon as it is online and idle (SweepAutoPark).
func (m *LightMonitor) SweepRefresh(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	var inFlight int
	if err := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspaces
		WHERE light_state IN ('opening', 'open') AND light_opened_by = 'refresh' AND status != 'removed'`,
	).Scan(&inFlight); err != nil {
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
	for _, r := range rows {
		id := r[0]
		res.Considered++
		lock := lightOpLock(id)
		if !lock.TryLock() {
			continue
		}
		_, _, _, ref := m.wh.openLight(ctx, id, lightTriggerRefresh)
		lock.Unlock()
		if ref != nil {
			log.Printf("LightMonitor: ws=%s refresh open refused: %d %s", id, ref.Status, ref.code())
			recordLightError(ctx, id, ref.code(), fmt.Sprint(ref.Body["error"]))
			res.Errors++
			continue
		}
		res.Acted++
	}
	return res
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
	for _, r := range rows {
		id := r[0]
		res.Considered++
		lock := lightOpLock(id)
		if !lock.TryLock() {
			continue
		}
		_, err := m.wh.deleteLightObjects(ctx, id)
		if err == nil {
			clearLightColumnsAfterDelete(ctx, id)
			res.Acted++
		} else {
			log.Printf("LightMonitor: ws=%s deleting the Light objects of a removed workspace: %v (retried)", id, err)
			res.Errors++
		}
		lock.Unlock()
	}
	return res
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
// overwritten. Returns the schedules that saw a failed or timed-out run.
func recordLightJobs(ctx context.Context, workspaceID string, jobs []provisioner.LightJob) (failedSchedules []string, unfinished bool) {
	seen := map[string]bool{}
	for _, j := range jobs {
		if !lightRunIDRe.MatchString(j.RunID) || !validLightScheduleName(j.Schedule) {
			continue
		}
		scheduledFor := parseLightJobTime(j.ScheduledFor)
		started := parseLightJobTime(j.StartedAt)
		switch j.Phase {
		case "pending":
			unfinished = true
		case "running":
			unfinished = true
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

// SweepRuns reads each Light agent's jobs listing: runs that never reported
// are recorded from their Jobs, failure streaks are evaluated, and a retired
// run token is revoked once no run is unfinished.
func (m *LightMonitor) SweepRuns(ctx context.Context) LightSweepResult {
	var res LightSweepResult
	if m.wh.lightAPI == nil {
		return res
	}
	rows, err := queryLightIDs(ctx, `
		SELECT w.id, COALESCE(w.light_retired_token_id::text, '') FROM workspaces w
		WHERE w.light_state IS NOT NULL AND w.status != 'removed'
		  AND EXISTS (SELECT 1 FROM light_schedules s WHERE s.workspace_id = w.id)`)
	if err != nil {
		log.Printf("LightMonitor: runs query failed: %v", err)
		res.Errors++
		return res
	}
	for _, r := range rows {
		id, retired := r[0], r[1]
		res.Considered++
		jctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
		jobs, err := m.wh.lightAPI.ListLightJobs(jctx, id)
		cancel()
		if err != nil {
			log.Printf("LightMonitor: ws=%s jobs listing failed: %v", id, err)
			res.Errors++
			continue
		}
		failed, unfinished := recordLightJobs(ctx, id, jobs)
		for _, schedule := range failed {
			if _, err := m.wh.evaluateLightStreak(ctx, id, schedule); err != nil {
				log.Printf("LightMonitor: ws=%s schedule %q streak: %v", id, schedule, err)
				res.Errors++
			}
		}
		if retired != "" && !unfinished {
			if revokeRetiredLightToken(ctx, id, retired) {
				res.Acted++
			}
		}
	}
	return res
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

// evaluateLightStreak suspends a schedule whose last three terminal runs
// (cancelled ones excluded; only runs since its last suspension) all failed:
// the control plane suspends its CronJob first; then core claims the
// suspension once and tells the owner. Nothing is written when the control
// plane refuses, so the next evaluation retries.
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
// schedule was suspended. When the agent may not talk to the user, the canvas
// still gets a WORKSPACE_UPDATED naming the schedule (GET /light shows it).
func (h *WorkspaceHandler) notifyLightScheduleSuspended(ctx context.Context, workspaceID, schedule, lastError string) {
	last := strings.TrimSpace(lastError)
	if last == "" {
		last = "none recorded"
	}
	last = headUTF8(last, 300)
	msg := fmt.Sprintf("Light: the schedule %q failed %d runs in a row, so it is suspended. Last error: %s. "+
		"Fix the cause, then park the agent from its Light settings to resume the schedule (or change the schedule).",
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
