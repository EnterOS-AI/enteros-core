// Package envx holds tiny helpers for reading tunable values from
// environment variables with a safe default. Named `envx` rather than
// `env` to avoid collision with Go's net/http and common third-party
// packages that use `env` as a var/import name.
//
// Rules of thumb for the helpers:
//   - Unset variable  → default
//   - Unparseable     → default (never crash startup)
//   - Parsed but ≤ 0  → default (a "disabled" override is almost always
//     a misconfiguration; use a feature flag instead)
package envx

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration reads `name` as a time.Duration string (e.g. "30s", "5m").
// Returns `def` when unset, unparseable, or non-positive.
func Duration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// Int64 reads `name` as a base-10 int64. Returns `def` when unset,
// unparseable, or non-positive.
func Int64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Bool reads `name` as a boolean. Returns `def` when unset or unparseable.
// Truthy values per strconv.ParseBool: 1, t, T, TRUE, true, True.
// Falsy: 0, f, F, FALSE, false, False, empty.
// All other values (including "yes", "on", "y", "n") are unparseable and
// return def. Use Bool for feature flags where the operator's mental
// model is "set truthy to enable" — a misconfigured value falls through
// to the safe default rather than silently enabling a feature.
func Bool(name string, def bool) bool {
	if v := os.Getenv(name); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// killSwitchOff is the set of values an operator may use to turn a shipped
// capability OFF. Compared case-insensitively after trimming whitespace.
var killSwitchOff = map[string]bool{
	"0": true, "f": true, "false": true,
	"n": true, "no": true, "off": true,
	"disable": true, "disabled": true,
}

// Enabled reports whether a SHIPPED capability guarded by `name` is on. It is
// the inverse polarity of Bool, for the opposite operator contract:
//
//	Bool(name, false) → "a dormant feature; set truthy to switch it on"
//	Enabled(name)     → "shipped capability; set falsy to switch it OFF"
//
// Unset — production's actual state for a variable nobody has ever configured
// — returns true. So does empty, a truthy value, and anything unrecognised: a
// typo in an operator's env must fail OPEN toward the capability the product
// ships, never silently dark-ship it. Only an explicit falsy value (0, f,
// false, n, no, off, disable, disabled — any case, whitespace trimmed) is a
// kill-switch.
//
// The kill-switch exists so an operator can revert a capability WITHOUT a
// redeploy. This is the same shape the *_SWEEPER_DISABLED flags and
// MOLECULE_MANIFEST_SSOT_ENFORCE=off already use in this repo; Enabled makes
// it one tested helper instead of a per-site hand-rolled string compare.
func Enabled(name string) bool {
	return !killSwitchOff[strings.ToLower(strings.TrimSpace(os.Getenv(name)))]
}
