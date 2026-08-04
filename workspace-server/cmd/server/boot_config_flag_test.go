package main

import (
	"os"
	"testing"
)

func unsetEnvForTest(t *testing.T, name string) {
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

// The CORE-served boot-config path (no R2, no CP dependency) is shipped
// capability. With NOTHING configured the token store must be created, which
// is what makes the boot-config endpoint serve instead of 404.
//
// UNSET is the load-bearing case: MOLECULE_BOOT_CONFIG_ENABLE appears in no
// tenant container env and no Infisical CP path, so "unset" IS production.
func TestBootConfigEnabled_DefaultsOnWhenUnset(t *testing.T) {
	unsetEnvForTest(t, "MOLECULE_BOOT_CONFIG_ENABLE")
	if _, present := os.LookupEnv("MOLECULE_BOOT_CONFIG_ENABLE"); present {
		t.Fatal("precondition failed: MOLECULE_BOOT_CONFIG_ENABLE is still set")
	}
	if !bootConfigEnabled() {
		t.Error("boot-config token delivery must be ON with the env var UNSET")
	}
}

func TestBootConfigEnabled_KillSwitchDisables(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "off", " off "} {
		t.Setenv("MOLECULE_BOOT_CONFIG_ENABLE", v)
		if bootConfigEnabled() {
			t.Errorf("MOLECULE_BOOT_CONFIG_ENABLE=%q must disable boot-config token delivery", v)
		}
	}
}

func TestBootConfigEnabled_ExplicitTrueStillEnables(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("MOLECULE_BOOT_CONFIG_ENABLE", v)
		if !bootConfigEnabled() {
			t.Errorf("MOLECULE_BOOT_CONFIG_ENABLE=%q must keep boot-config token delivery ON", v)
		}
	}
}
