package handlers

import "testing"

// The provision-time overlay is new behaviour on the Create path — the most
// blast-radius-sensitive code in the server — so it ships DARK. With the flag
// off, provisioning must be byte-identical to before.
func TestPluginSettingsLayers_DefaultsOff(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	if pluginSettingsLayersEnabled() {
		t.Error("must default OFF — a new Create-path behaviour must not switch on at merge")
	}
}

func TestPluginSettingsLayers_TruthyValuesEnable(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", v)
		if !pluginSettingsLayersEnabled() {
			t.Errorf("%q should enable the overlay", v)
		}
	}
}

// Anything unrecognised is OFF, not on. A typo in an operator's env must fail
// safe — toward today's behaviour — rather than silently enabling new
// provision-path work.
func TestPluginSettingsLayers_UnrecognisedValuesStayOff(t *testing.T) {
	for _, v := range []string{"0", "false", "no", "off", "garbage", "tru"} {
		t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", v)
		if pluginSettingsLayersEnabled() {
			t.Errorf("%q must NOT enable the overlay", v)
		}
	}
}
