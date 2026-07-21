package handlers

import (
	"os"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
)

// TestNativeRegistry_SourcesByteIdenticalToRetiredConsts is the migration-safety
// gate: consuming the SDK native-plugins registry must resolve to the EXACT
// source strings core previously hard-coded, or the boot-install would suddenly
// fetch a different ref/repo. These literals are the retired constants
// (SchedulerPluginSource, conciergePlatformMCPSource) frozen here on purpose — a
// registry edit that changes either reaches core through a molcontracts bump and
// trips this test, forcing a deliberate review rather than a silent behavior
// change.
func TestNativeRegistry_SourcesByteIdenticalToRetiredConsts(t *testing.T) {
	cases := []struct {
		name       string
		got        string
		wantSource string
	}{
		{SchedulerPluginName, SchedulerPluginSource, "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"},
		{conciergePlatformMCPName, conciergePlatformMCPSource, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"},
	}
	for _, c := range cases {
		if c.got != c.wantSource {
			t.Errorf("%s: registry source = %q, want the retired literal %q (registry drifted from what core boot-installs)", c.name, c.got, c.wantSource)
		}
		// mustNativePluginSource(name) must agree with the package var it seeded.
		if ms := mustNativePluginSource(c.name); ms != c.got {
			t.Errorf("%s: mustNativePluginSource = %q, disagrees with the package var %q", c.name, ms, c.got)
		}
	}
}

// TestNativeRegistry_ConciergeNameDerivesFromRegistrySource pins the entitlement
// gate's invariant: recordDeclaredPlugin matches the privileged plugin by the
// literal conciergePlatformMCPName, so the registry source MUST derive exactly
// that name via PluginNameFromSource. If a registry rename made the derived name
// diverge, the "install:concierge only on kind=platform" gate would silently
// stop matching.
func TestNativeRegistry_ConciergeNameDerivesFromRegistrySource(t *testing.T) {
	got, err := plugins.PluginNameFromSource(conciergePlatformMCPSource)
	if err != nil {
		t.Fatalf("PluginNameFromSource(%q): %v", conciergePlatformMCPSource, err)
	}
	if got != conciergePlatformMCPName {
		t.Fatalf("registry concierge source derives name %q, but the entitlement gate matches %q — gate would stop firing", got, conciergePlatformMCPName)
	}
}

// TestNativeRegistry_DefaultSetExcludesConcierge proves the install-policy split
// the delivery depends on: defaultNativePluginSources() (declared on EVERY
// workspace) must contain the scheduler but NEVER the privileged concierge MCP —
// which is install:concierge and stays gated to the org-root platform workspace.
func TestNativeRegistry_DefaultSetExcludesConcierge(t *testing.T) {
	defaults := defaultNativePluginSources()
	if len(defaults) == 0 {
		t.Fatal("defaultNativePluginSources() is empty — the registry lost every install:default entry")
	}
	var sawScheduler bool
	for _, s := range defaults {
		if s == conciergePlatformMCPSource {
			t.Errorf("install:default set contains the privileged concierge MCP source %q — it must be install:concierge only", s)
		}
		if s == SchedulerPluginSource {
			sawScheduler = true
		}
	}
	if !sawScheduler {
		t.Errorf("install:default set is missing the scheduler %q; got %v", SchedulerPluginSource, defaults)
	}
}

// TestNativeRegistry_DefaultSetIncludesDigestProviders proves the digest RFC's
// delivery payload is present: the four idle-digest plugins are declared as
// install:default so the fleet rollout (flag on) reaches every workspace.
func TestNativeRegistry_DefaultSetIncludesDigestProviders(t *testing.T) {
	// Golden set, bumped consciously when the SDK registry pins move (a molcontracts
	// bump trips this, forcing a deliberate review — same discipline as
	// TestNativeRegistry_SourcesByteIdenticalToRetiredConsts). These pins are the
	// digest-provider source-move (RFC molecule-core#4413 D3, v0.2.x). Their
	// declaration is still flag-gated OFF (declareDefaultNativePluginsEnabled), so
	// the pin bump is dormant on the fleet until Phase-B arming flips the flag.
	want := []string{
		"gitea://molecule-ai/molecule-ai-plugin-digest-goal#v0.2.0",
		"gitea://molecule-ai/molecule-ai-plugin-digest-identity#v0.2.1",
		"gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.2.1",
		"gitea://molecule-ai/molecule-ai-plugin-digest-task-queue#v0.2.0",
	}
	defaults := defaultNativePluginSources()
	have := map[string]bool{}
	for _, s := range defaults {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("install:default set is missing digest provider %q; got %v", w, defaults)
		}
	}
}

// TestDeclareDefaultNativePluginsEnabled_DefaultOff is the blast-radius gate:
// with the flag unset the universal declaration is a no-op, so merging the
// consumer changes nothing on the fleet. The truthy values arm it.
func TestDeclareDefaultNativePluginsEnabled_DefaultOff(t *testing.T) {
	t.Setenv(declareDefaultNativePluginsEnv, "")
	os.Unsetenv(declareDefaultNativePluginsEnv)
	if declareDefaultNativePluginsEnabled() {
		t.Fatal("flag unset must be OFF (default-off blast-radius guarantee)")
	}
	for _, off := range []string{"", "0", "false", "no", "FALSE", "No"} {
		t.Setenv(declareDefaultNativePluginsEnv, off)
		if declareDefaultNativePluginsEnabled() {
			t.Errorf("value %q must read as OFF", off)
		}
	}
	for _, on := range []string{"1", "true", "yes", "TRUE", "on"} {
		t.Setenv(declareDefaultNativePluginsEnv, on)
		if !declareDefaultNativePluginsEnabled() {
			t.Errorf("value %q must read as ON", on)
		}
	}
}

// TestDefaultNativePluginSourcesForDeclare_ExcludesScheduler proves the fix for
// the latent double-declare: declareDefaultNativePlugins seeds
// defaultNativePluginSourcesForDeclare(), which MUST drop the scheduler source
// (the dedicated ensureSchedulerPluginDeclared path owns it), while leaving the
// registry SSOT (defaultNativePluginSources) untouched.
//
// Negative control: the pre-fix declareDefaultNativePlugins seeded
// defaultNativePluginSources() directly, which DOES contain SchedulerPluginSource
// (asserted below via the raw set) — so the pre-fix seed set would fail the
// "scheduler absent" assertion here.
func TestDefaultNativePluginSourcesForDeclare_ExcludesScheduler(t *testing.T) {
	forDeclare := defaultNativePluginSourcesForDeclare()
	for _, s := range forDeclare {
		if s == SchedulerPluginSource {
			t.Fatalf("declareDefaultNativePlugins seed set still contains the scheduler source %q — the ensureSchedulerPluginDeclared path owns it; a second (differently-named) row = duplicate boot-install", s)
		}
	}
	// The registry SSOT is NOT mutated: the raw default set still carries it.
	var rawHasScheduler bool
	for _, s := range defaultNativePluginSources() {
		if s == SchedulerPluginSource {
			rawHasScheduler = true
		}
	}
	if !rawHasScheduler {
		t.Fatalf("filtering must happen in declareDefaultNativePlugins only — defaultNativePluginSources() (registry SSOT) must still include the scheduler")
	}
	// The digest providers survive the filter (only the scheduler is dropped).
	if len(forDeclare) != len(defaultNativePluginSources())-1 {
		t.Fatalf("expected exactly one source (the scheduler) filtered out; declare-set=%d raw=%d", len(forDeclare), len(defaultNativePluginSources()))
	}
}

// TestScheduler_DeclaredExactlyOnce_AcrossProvisionDeclarePaths pins the property
// finding #3b is about: across the TWO provision-time declare paths — the
// dedicated ensureSchedulerPluginDeclared (name SchedulerPluginName) and the
// flag-armed declareDefaultNativePlugins (name derived from source) — the
// scheduler plugin is declared under exactly ONE name, i.e. exactly once.
//
// Negative control: with the pre-fix seed set (defaultNativePluginSources(),
// unfiltered) the derived scheduler name "molecule-ai-plugin-scheduler" AND the
// const SchedulerPluginName "molecule-scheduler" would BOTH be present, giving a
// count of 2 — this test would fail. (The two names differing is itself asserted.)
func TestScheduler_DeclaredExactlyOnce_AcrossProvisionDeclarePaths(t *testing.T) {
	derived, err := plugins.PluginNameFromSource(SchedulerPluginSource)
	if err != nil {
		t.Fatalf("PluginNameFromSource(%q): %v", SchedulerPluginSource, err)
	}
	// The whole hazard rests on the two names diverging; assert that explicitly.
	if derived == SchedulerPluginName {
		t.Fatalf("test premise broken: derived name %q == const name %q (no double-declare hazard)", derived, SchedulerPluginName)
	}

	names := map[string]int{}
	// Path 1: ensureSchedulerPluginDeclared always records under the const name.
	names[SchedulerPluginName]++
	// Path 2: declareDefaultNativePlugins (flag armed) records each seed source
	// under its derived name.
	for _, s := range defaultNativePluginSourcesForDeclare() {
		n, err := plugins.PluginNameFromSource(s)
		if err != nil {
			t.Fatalf("PluginNameFromSource(%q): %v", s, err)
		}
		names[n]++
	}

	total := names[SchedulerPluginName] + names[derived]
	if total != 1 {
		t.Fatalf("scheduler declared %d time(s) across provision declare paths (%q=%d, %q=%d); want exactly 1 — a second row = duplicate boot-install",
			total, SchedulerPluginName, names[SchedulerPluginName], derived, names[derived])
	}
}

// TestDeclareSchedulerPluginEnabled_DefaultOn is the kill-switch blast-radius gate
// (finding #3a): the unconditional per-provision scheduler declare stays ON by
// default (unset/"" enabled, byte-identical to today), and only an explicit falsey
// value disables it — the inverse of declareDefaultNativePluginsEnabled.
func TestDeclareSchedulerPluginEnabled_DefaultOn(t *testing.T) {
	os.Unsetenv(declareSchedulerPluginEnv)
	if !declareSchedulerPluginEnabled() {
		t.Fatal("flag unset must be ON (default-on kill-switch: provisioning unchanged unless explicitly disabled)")
	}
	for _, on := range []string{"", "1", "true", "yes", "on", "TRUE", "whatever"} {
		t.Setenv(declareSchedulerPluginEnv, on)
		if !declareSchedulerPluginEnabled() {
			t.Errorf("value %q must read as ON (default-on)", on)
		}
	}
	for _, off := range []string{"0", "false", "no", "FALSE", "No", " false "} {
		t.Setenv(declareSchedulerPluginEnv, off)
		if declareSchedulerPluginEnabled() {
			t.Errorf("value %q must read as OFF (explicit disable)", off)
		}
	}
}

// TestMustNativePluginSource_PanicsOnMissing proves the fail-loud contract: a
// name the registry doesn't carry panics rather than returning "".
func TestMustNativePluginSource_PanicsOnMissing(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("mustNativePluginSource on a missing name must panic, not return empty")
		}
	}()
	_ = mustNativePluginSource("molecule-ai-plugin-does-not-exist")
}
