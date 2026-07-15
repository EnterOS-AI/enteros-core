// Package cronspec is the Go implementation of the Molecule `cron` contract
// (molecule-ai-sdk contracts/cron) — the shared SSOT for cron grammar and
// next-run semantics. It is a thin, deliberately behaviour-preserving wrapper
// over robfig/cron v3's standard 5-field parser: the reference the contract's
// fixture set is generated from, and therefore the behaviour every existing
// schedule already depends on.
//
// This package is the single home for cron math in workspace-server. The
// scheduler engine and the schedule CRUD/seed handlers all call
// ComputeNextRun/Validate here (previously duplicated in internal/scheduler).
// The runtime trigger-plugin daemon implements the same contract in Python
// (molecule_runtime/cronspec); cronspec_conformance_test.go and the runtime's
// test_cronspec_contract.py are the two ends of the cross-language equivalence
// gate, both asserting the shared fixtures.json.
package cronspec

import (
	"fmt"
	"time"

	cronlib "github.com/robfig/cron/v3"
)

// MaxExprLen bounds a cron expression (contract: bounds.max_expr_len_chars).
// Mirrors the per-template cap enforced at schedule ingestion.
const MaxExprLen = 128

// standardParser is the 5-field standard parser: minute hour day-of-month month
// day-of-week — no seconds field, no @-descriptors. This exact flag set is the
// contract; changing it changes behaviour for every schedule.
var standardParser = cronlib.NewParser(
	cronlib.Minute | cronlib.Hour | cronlib.Dom | cronlib.Month | cronlib.Dow,
)

// ComputeNextRun parses a 5-field cron expression and returns the next fire
// time strictly after `after`, evaluated in timezone `tz` (IANA name) and
// returned in UTC. Errors on an invalid timezone or expression.
func ComputeNextRun(cronExpr, tz string, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	sched, err := standardParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return sched.Next(after.In(loc)).UTC(), nil
}

// Validate reports whether the expression + timezone are well-formed, without
// computing a fire time. Used by the CRUD handlers to reject bad input at write.
func Validate(cronExpr, tz string) error {
	if len(cronExpr) > MaxExprLen {
		return fmt.Errorf("cron expression exceeds %d chars", MaxExprLen)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	if _, err := standardParser.Parse(cronExpr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return nil
}
