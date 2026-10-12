package cronspec

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// fixture mirrors one row of the SDK cron contract's fixtures.json — the
// executable behavioural SSOT (contracts/cron/fixtures.json, vendored into
// testdata/). This test is the Go end of the cross-language equivalence gate:
// it proves the shipping robfig/cron behaviour still matches the contract, so a
// robfig version bump that silently changed a schedule's fire time reds CI.
type fixture struct {
	Desc   string `json:"desc"`
	Expr   string `json:"expr"`
	TZ     string `json:"tz"`
	After  string `json:"after"`
	Expect string `json:"expect"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fs []fixture
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("no fixtures loaded")
	}
	return fs
}

func TestComputeNextRun_ConformsToContractFixtures(t *testing.T) {
	for _, f := range loadFixtures(t) {
		after, err := time.Parse(time.RFC3339, f.After)
		if err != nil {
			t.Fatalf("%s: bad after %q: %v", f.Desc, f.After, err)
		}
		want, err := time.Parse(time.RFC3339, f.Expect)
		if err != nil {
			t.Fatalf("%s: bad expect %q: %v", f.Desc, f.Expect, err)
		}
		got, err := ComputeNextRun(f.Expr, f.TZ, after)
		if err != nil {
			t.Errorf("%s: ComputeNextRun(%q,%q) error: %v", f.Desc, f.Expr, f.TZ, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%s: %q @ %s after %s\n  got  %s\n  want %s",
				f.Desc, f.Expr, f.TZ, f.After, got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

func TestValidate(t *testing.T) {
	good := []struct{ expr, tz string }{
		{"*/15 * * * *", "UTC"},
		{"0 9 * * MON", "America/New_York"},
		{"0 0 1 JAN *", "Asia/Kolkata"},
	}
	for _, c := range good {
		if err := Validate(c.expr, c.tz); err != nil {
			t.Errorf("Validate(%q,%q) unexpected error: %v", c.expr, c.tz, err)
		}
	}
	bad := []struct {
		name, expr, tz string
	}{
		{"too few fields", "* * * *", "UTC"},
		{"dow 7 rejected", "0 9 * * 7", "UTC"},
		{"minute out of range", "60 * * * *", "UTC"},
		{"bad tz", "* * * * *", "Mars/Phobos"},
		{"over length", "* * * * * ", "UTC"}, // trailing space still parses; length ok — replaced below
	}
	for _, c := range bad {
		if c.name == "over length" {
			long := ""
			for i := 0; i < 130; i++ {
				long += "0"
			}
			if err := Validate(long+" * * * *", "UTC"); err == nil {
				t.Errorf("Validate over-length: expected error")
			}
			continue
		}
		if err := Validate(c.expr, c.tz); err == nil {
			t.Errorf("Validate(%q,%q) [%s]: expected error, got nil", c.expr, c.tz, c.name)
		}
	}
}
