package handlers

// M6 / FIX 3: a node that references a template inherits that template's
// plugins.
//
// Today mergePlugins unions org DEFAULTS with the NODE's list and never looks
// at the referenced template — so `template: seo-agent` does not bring
// seo-agent's own plugins, which is the main thing a template knows.

import (
	"strings"
	"testing"
)

// Both forms a template may use: bare strings, and the {source, config} object
// a template writes once it sets per-install config (sdk#176).
const templateWithPlugins = `
name: SEO Agent
runtime: claude-code
plugins:
  - gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc9
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      poll_seconds: 15
  - seo-all
`

func TestTemplatePlugins_ReadsBothBareAndObjectForms(t *testing.T) {
	got := templateDeclaredPlugins([]byte(templateWithPlugins))
	want := []string{
		"gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc9",
		"gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0",
		"seo-all",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d plugins %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("plugin %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTemplatePlugins_MalformedOrAbsentYieldsNothing(t *testing.T) {
	for _, y := range []string{
		"", "name: x\n", "plugins: not-a-list\n", "plugins: []\n",
		"plugins:\n  - {}\n", "plugins:\n  - source: \"\"\n", "name: [unclosed",
	} {
		if got := templateDeclaredPlugins([]byte(y)); len(got) != 0 {
			t.Errorf("%q yielded %v; a template that cannot be read must not fail an import", y, got)
		}
	}
}

// THE FIX: template plugins arrive, and precedence is template → defaults → node.
func TestTemplatePlugins_ThreeLayerPrecedence(t *testing.T) {
	got := mergePluginsWithTemplate(
		templateDeclaredPlugins([]byte(templateWithPlugins)),
		[]string{"ecc"},
		[]string{"molecule-audit"},
	)
	joined := strings.Join(got, ",")
	for _, must := range []string{"seo-all", "ecc", "molecule-audit", "molecule-ai-plugin-scheduler"} {
		if !strings.Contains(joined, must) {
			t.Errorf("%q missing from the merged set: %v", must, got)
		}
	}
	// Template first, then defaults, then node — a stable order matters because
	// the install name of the FIRST source to claim a name wins on collision.
	if got[0] != "gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc9" {
		t.Errorf("template plugins should lead: %v", got)
	}
	if got[len(got)-1] != "molecule-audit" {
		t.Errorf("node plugins should come last: %v", got)
	}
}

// Inheriting is only safe if a node can DECLINE one.
func TestTemplatePlugins_NodeCanOptOutOfAnInheritedPlugin(t *testing.T) {
	got := mergePluginsWithTemplate(
		[]string{"seo-all", "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"},
		nil,
		[]string{"!seo-all"},
	)
	for _, p := range got {
		if p == "seo-all" {
			t.Fatalf("node opted out of seo-all but it survived: %v", got)
		}
	}
	if len(got) != 1 {
		t.Errorf("the un-declined template plugin should remain: %v", got)
	}
}

func TestTemplatePlugins_NoTemplateIsTodaysBehaviourExactly(t *testing.T) {
	defaults := []string{"ecc", "molecule-audit"}
	node := []string{"superpowers", "!ecc"}
	withTemplate := mergePluginsWithTemplate(nil, defaults, node)
	todays := mergePlugins(defaults, node)
	if strings.Join(withTemplate, ",") != strings.Join(todays, ",") {
		t.Errorf("with no template the result must be byte-identical to today:\n got %v\nwant %v", withTemplate, todays)
	}
}

func TestTemplatePlugins_DuplicatesAcrossLayersCollapse(t *testing.T) {
	got := mergePluginsWithTemplate([]string{"seo-all"}, []string{"seo-all"}, []string{"seo-all"})
	if len(got) != 1 {
		t.Errorf("the same source declared in all three layers should appear once: %v", got)
	}
}
