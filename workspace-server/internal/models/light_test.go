package models

import "testing"

func TestLightBlocksWakeOnlyWhileParkingOrParked(t *testing.T) {
	for state, want := range map[string]bool{
		"":           false,
		LightOpen:    false,
		LightOpening: false,
		LightParking: true,
		LightParked:  true,
	} {
		if got := LightBlocksWake(state); got != want {
			t.Errorf("LightBlocksWake(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestLightWakeAllowedSQLIsOnePredicate(t *testing.T) {
	if LightWakeAllowedSQL != `COALESCE(light_state, '') NOT IN ('parking', 'parked')` {
		t.Errorf("LightWakeAllowedSQL = %q", LightWakeAllowedSQL)
	}
	if got := LightWakeAllowedSQLFor(""); got != LightWakeAllowedSQL {
		t.Errorf("no alias: %q", got)
	}
	if got := LightWakeAllowedSQLFor("w"); got != `COALESCE(w.light_state, '') NOT IN ('parking', 'parked')` {
		t.Errorf("alias w: %q", got)
	}
}
