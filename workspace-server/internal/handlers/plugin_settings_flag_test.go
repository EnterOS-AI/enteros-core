package handlers

import (
	"os"
	"testing"
)

// The layer-6 provision-time overlay is shipped capability: without it an
// operator override survives in the DATABASE but a re-provisioned box still
// runs the template value, so M4's done-condition is not met on the box.
//
// UNSET is the load-bearing case — MOLECULE_PLUGIN_SETTINGS_LAYERS appears in
// no tenant container env and no Infisical CP path, so "unset" IS production.
func TestPluginSettingsLayers_DefaultsOnWhenUnset(t *testing.T) {
	unsetEnvForTest(t, "MOLECULE_PLUGIN_SETTINGS_LAYERS")
	if _, present := os.LookupEnv("MOLECULE_PLUGIN_SETTINGS_LAYERS"); present {
		t.Fatal("precondition failed: MOLECULE_PLUGIN_SETTINGS_LAYERS is still set")
	}
	if !pluginSettingsLayersEnabled() {
		t.Error("the layer-6 overlay must be ON with the env var UNSET — it is shipped capability")
	}
}

func TestPluginSettingsLayers_DefaultsOnWhenEmpty(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	if !pluginSettingsLayersEnabled() {
		t.Error("a SET-but-empty value must leave the overlay ON")
	}
}

func TestPluginSettingsLayers_TruthyValuesStayOn(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", v)
		if !pluginSettingsLayersEnabled() {
			t.Errorf("%q should keep the overlay on", v)
		}
	}
}

// The variable survives as an operator kill-switch: an explicit falsy value
// reverts the Create path to delivering the rendered template values, with no
// redeploy required.
func TestPluginSettingsLayers_KillSwitchDisables(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "off", " off "} {
		t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", v)
		if pluginSettingsLayersEnabled() {
			t.Errorf("%q must disable the overlay (operator kill-switch)", v)
		}
	}
}

// A typo must fail OPEN toward the shipped capability rather than silently
// dark-shipping it — only an explicit falsy value is a kill-switch.
func TestPluginSettingsLayers_UnrecognisedValuesStayOn(t *testing.T) {
	for _, v := range []string{"garbage", "tru", "offf", "-1"} {
		t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", v)
		if !pluginSettingsLayersEnabled() {
			t.Errorf("%q must NOT disable the overlay", v)
		}
	}
}
