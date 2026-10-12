package envx

import (
	"os"
	"testing"
)

// unsetForTest genuinely REMOVES the variable for the duration of the test and
// restores whatever was there before. t.Setenv(name, "") is NOT the same thing:
// it leaves the variable SET-but-empty. Production's actual state for a
// never-configured flag is UNSET, and that is the state these tests must
// exercise — this repo has repeatedly shipped bugs where only the injected
// value was covered and the default never was.
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	prev, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetenv %s: %v", name, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func TestEnabled_UnsetIsOn(t *testing.T) {
	const key = "__envx_test_enabled_unset"
	unsetForTest(t, key)
	if _, present := os.LookupEnv(key); present {
		t.Fatalf("precondition failed: %s is still present in the environment", key)
	}
	if !Enabled(key) {
		t.Error("an UNSET kill-switch variable must leave the capability ON — this is production's actual state")
	}
}

func TestEnabled_EmptyIsOn(t *testing.T) {
	const key = "__envx_test_enabled_empty"
	t.Setenv(key, "")
	if !Enabled(key) {
		t.Error("a SET-but-empty kill-switch variable must leave the capability ON")
	}
}

func TestEnabled_ExplicitDisableValuesTurnItOff(t *testing.T) {
	const key = "__envx_test_enabled_off"
	for _, v := range []string{
		"0", "f", "F", "false", "FALSE", "False",
		"n", "N", "no", "NO", "off", "OFF", "Off",
		"disable", "disabled", "DISABLED",
		" false ", "\toff\n",
	} {
		t.Setenv(key, v)
		if Enabled(key) {
			t.Errorf("%q must disable the capability (operator kill-switch)", v)
		}
	}
}

func TestEnabled_TruthyAndUnrecognisedStayOn(t *testing.T) {
	const key = "__envx_test_enabled_on"
	// Truthy values keep it on (an operator who explicitly opts in gets what
	// they asked for), and so does anything unrecognised: a typo must fail
	// OPEN toward the shipped capability rather than silently dark-shipping
	// it. This is the opposite polarity from Bool, deliberately.
	for _, v := range []string{"1", "t", "true", "TRUE", "yes", "on", "enabled", "garbage", "fals", "offf", "-1"} {
		t.Setenv(key, v)
		if !Enabled(key) {
			t.Errorf("%q must NOT disable the capability — only an explicit falsy value is a kill-switch", v)
		}
	}
}
