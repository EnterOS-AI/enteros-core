package handlers

// A widened type is only as wide as its NARROWEST decoder.
//
// #4944 changed OrgDefaults/OrgWorkspace.Plugins from []string to
// []templatePluginEntry and gave the type an UnmarshalYAML only. `plugins:`
// arrives as YAML from a template FILE but as JSON from POST /org/import with
// an inline template — so the inline path began rejecting the bare-string form
// every fleet template uses, with the whole import 400'ing "invalid request
// body". Unit tests over the YAML path stayed green throughout.

import (
	"encoding/json"
	"testing"
)

func TestPluginEntryJSON_BareStringForm(t *testing.T) {
	var d OrgDefaults
	if err := json.Unmarshal([]byte(`{"plugins":["browser-automation","ecc"]}`), &d); err != nil {
		t.Fatalf("bare-string form must decode from JSON (this is what broke /org/import): %v", err)
	}
	if len(d.Plugins) != 2 || d.Plugins[0].Source != "browser-automation" || d.Plugins[1].Source != "ecc" {
		t.Fatalf("wrong decode: %+v", d.Plugins)
	}
	if d.Plugins[0].Config != nil {
		t.Fatalf("a bare string carries no config")
	}
}

func TestPluginEntryJSON_ObjectForm(t *testing.T) {
	var w OrgWorkspace
	err := json.Unmarshal([]byte(`{"plugins":[{"source":"gitea://o/r#v1","config":{"schedules":[{"name":"H"}]}}]}`), &w)
	if err != nil {
		t.Fatalf("object form: %v", err)
	}
	if len(w.Plugins) != 1 || w.Plugins[0].Source != "gitea://o/r#v1" {
		t.Fatalf("wrong source: %+v", w.Plugins)
	}
	if _, ok := w.Plugins[0].Config["schedules"]; !ok {
		t.Fatalf("config dropped: %+v", w.Plugins[0].Config)
	}
}

func TestPluginEntryJSON_MixedListInOneDocument(t *testing.T) {
	// The realistic shape: org defaults are bare strings, a node adds a
	// configured entry.
	var w OrgWorkspace
	if err := json.Unmarshal([]byte(`{"plugins":["ecc",{"source":"s","config":{"a":1}},"!seo"]}`), &w); err != nil {
		t.Fatalf("mixed list: %v", err)
	}
	if len(w.Plugins) != 3 || w.Plugins[0].Source != "ecc" || w.Plugins[2].Source != "!seo" {
		t.Fatalf("mixed decode wrong: %+v", w.Plugins)
	}
}

func TestPluginEntryJSON_ObjectMissingSourceIsRefused(t *testing.T) {
	var w OrgWorkspace
	if err := json.Unmarshal([]byte(`{"plugins":[{"config":{"a":1}}]}`), &w); err == nil {
		t.Fatalf("an entry with config but no source must be refused")
	}
}

func TestPluginEntryJSON_RoundTrip(t *testing.T) {
	// A re-serialised template must stay loadable: config-less entries collapse
	// back to a plain string rather than becoming {"source":...,"config":null}.
	in := []byte(`{"plugins":["ecc",{"source":"s","config":{"a":1}}]}`)
	var w OrgWorkspace
	if err := json.Unmarshal(in, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(w.Plugins)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back []templatePluginEntry
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-decode of our own output failed (%s): %v", out, err)
	}
	if len(back) != 2 || back[0].Source != "ecc" || back[1].Source != "s" {
		t.Fatalf("round-trip lost data: %s", out)
	}
	if string(out) == "" || out[1] != '"' {
		t.Fatalf("config-less entry did not collapse to a string: %s", out)
	}
}

func TestPluginEntryJSON_YAMLPathStillWorks(t *testing.T) {
	// Guard against "fixed JSON, broke YAML".
	var d OrgDefaults
	if err := yamlUnmarshalForTest([]byte("plugins:\n  - ecc\n  - source: s\n    config: {a: 1}\n"), &d); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if len(d.Plugins) != 2 || d.Plugins[0].Source != "ecc" || d.Plugins[1].Config == nil {
		t.Fatalf("yaml decode regressed: %+v", d.Plugins)
	}
}
