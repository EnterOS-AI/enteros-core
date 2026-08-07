package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gopkg.in/yaml.v3"
)

// concierge_identity_name_description_test.go — the concierge's composed
// config.yaml must carry the ORG CONCIERGE identity, not the base runtime
// template's.
//
// THE BUG (observed live on prod, 2026-08-06): composeConciergeRuntimeConfig
// neutralized runtime_config.required_env and grafted prompt_files, but copied
// the base runtime template's `name` and `description` VERBATIM. For the default
// hermes runtime those are:
//
//	name: Hermes Agent
//	description: Nous Research hermes-agent — the self-improving AI agent ...
//
// The runtime renders that description into the system prompt as the agent's
// Role, directly alongside the grafted Org Concierge persona. The prompt then
// carries TWO COMPETING IDENTITIES and the model picks one non-deterministically:
// a prod concierge with a correct, fully-substituted persona file on disk still
// answered "I am Hermes Agent, operated by Nous Research — a self-improving AI
// agent", quoting this description near-verbatim, while a sibling concierge on
// the same build answered correctly. That coin-flip is why "the persona bytes
// are on disk" kept reading as fixed while users still saw the wrong identity.
//
// Delivering the persona file is necessary but NOT sufficient — the competing
// identity has to be removed at the same time.
//
// SCOPE OF THE GUARANTEE (measured, not assumed — see
// TestComposeConciergeRuntimeConfig_RealTemplateRenderedIdentityIsClean):
// the product overrides exactly TWO fields, `name` and `description`, because
// those are the two the runtime renders as the agent's Role. It does NOT scrub
// the whole document. Against the REAL 477-line hermes template the composed
// output still contains the vendor's name inside YAML comments, provider notes
// and model DISPLAY names ("Hermes 4 70B (Nous Portal)"), preserved by the
// yaml.v3 Node round-trip. Those are not prompt material. Asserting a
// whole-document absence would be asserting a guarantee the product does not
// provide — it would hold only against a toy fixture and would be false the
// moment the test met the shipped template. Every assertion below is therefore
// on the RENDERED FIELDS.

// conciergeIdentitySchedulesBlock is a runtime-native `schedules:` node for the
// platform-agent fixture template. Its presence is load-bearing, not decoration:
// on self-host it makes graftConciergeSchedules actually GRAFT, which routes
// composeConciergeRuntimeConfig through its SECOND exit (a re-marshal of the
// whole doc) instead of the first. That grafted exit is the one every self-host
// concierge takes, and it was previously unasserted by these tests.
const conciergeIdentitySchedulesBlock = "schedules:\n" +
	"    - name: identity-daily-brief\n" +
	"      cron: 0 9 * * *\n" +
	"      timezone: UTC\n" +
	"      prompt: Post the daily brief.\n" +
	"      enabled: true\n"

// writeConciergeIdentityFixtures writes per-runtime base templates that carry a
// REAL vendor name+description, mirroring the shipped hermes template. The
// description text is the load-bearing part: it is what the runtime renders as
// the agent's Role.
//
// It ALSO writes platform-agent/config.yaml (with schedules) — without that file
// graftConciergeSchedules bails at its os.ReadFile and every test here silently
// measures only the ungrafted exit.
func writeConciergeIdentityFixtures(t *testing.T, configsDir string) {
	t.Helper()
	fixtures := map[string]string{
		"hermes/config.yaml": "name: Hermes Agent\n" +
			"description: >-\n" +
			"    Nous Research hermes-agent — the self-improving AI agent with built-in\n" +
			"    terminal, file ops, web search, memory, skills, and cross-session recall.\n" +
			"runtime: hermes\n" +
			"prompt_files:\n" +
			"- system-prompt.md\n" +
			"runtime_config:\n" +
			"  model: minimax/MiniMax-M2.7\n" +
			"  required_env: [MINIMAX_API_KEY]\n",
		"openclaw/config.yaml": "name: OpenClaw Agent\n" +
			"description: OpenClaw — a general-purpose coding agent.\n" +
			"runtime: openclaw\n" +
			"runtime_config:\n" +
			"  model: minimax:MiniMax-M2.7\n",
		"claude-code-default/config.yaml": "name: Claude Code Agent\n" +
			"description: Anthropic Claude Code — a terminal coding assistant.\n" +
			"runtime: claude-code\n" +
			"runtime_config:\n" +
			"  model: moonshot/kimi-k2.6\n" +
			"  required_env: [ANTHROPIC_API_KEY]\n",
		"platform-agent/config.yaml": "name: Org Concierge\n" +
			"runtime: claude-code\n" + conciergeIdentitySchedulesBlock,
		"platform-agent/prompts/concierge.md": "# You are {{CONCIERGE_NAME}} — the Org Concierge\n",
	}
	for rel, content := range fixtures {
		p := filepath.Join(configsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// conciergeIdentityFixtureHandler builds a handler over a fresh fixture tree and
// PINS the deployment mode.
//
// Every test in this file pins MOLECULE_ORG_ID explicitly, matching the sibling
// compose tests. It is not hygiene: it selects which of
// composeConciergeRuntimeConfig's TWO exits runs — self-host grafts schedules and
// re-marshals the document, SaaS returns the first marshal — so leaving it to the
// ambient environment would make WHICH code path is measured depend on the shell
// the suite happens to run in.
func conciergeIdentityFixtureHandler(t *testing.T, orgID string) *WorkspaceHandler {
	t.Helper()
	t.Setenv("MOLECULE_ORG_ID", orgID)
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	return &WorkspaceHandler{configsDir: dir}
}

// conciergeDeploymentModes drives every identity assertion down BOTH exits of
// composeConciergeRuntimeConfig. wantSchedules is the observable that proves
// which exit ran, so a matrix case can never pass while silently measuring the
// other path.
var conciergeDeploymentModes = []struct {
	name          string
	orgID         string
	wantSchedules bool
}{
	{"selfhost_schedules_grafted", "", true},
	{"saas_ungrafted", "org-saas-xyz", false},
}

// assertComposeExit fails unless the composed config took the exit the mode
// expects. This is the anti-vacuous control for the whole matrix: the grafted
// re-marshal is a genuinely different code path (yaml.Marshal runs a second
// time), and without this check a broken graft would quietly downgrade every
// "self-host" case into a duplicate of the SaaS one.
func assertComposeExit(t *testing.T, composed []byte, wantSchedules bool) {
	t.Helper()
	names := parseComposedScheduleNames(t, composed)
	if wantSchedules && len(names) == 0 {
		t.Fatalf("expected the SCHEDULES-GRAFTED exit (re-marshalled doc) but the composed config has no schedules — this case is measuring the wrong code path\n%s", composed)
	}
	if !wantSchedules && len(names) != 0 {
		t.Fatalf("expected the ungrafted exit but the composed config carries schedules %v", names)
	}
}

// parseComposedIdentity extracts top-level name/description for assertion.
func parseComposedIdentity(t *testing.T, cfg []byte) (string, string) {
	t.Helper()
	var doc struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("parse composed config: %v", err)
	}
	return doc.Name, doc.Description
}

// vendorIdentityMarkers are strings that must NOT survive into the RENDERED
// identity fields (`name`, `description`) of a concierge's composed config. Each
// is quoted from the shipped hermes template and each has been observed in a
// wrong self-identification on prod.
//
// They are deliberately NOT applied to the whole document: see the scope note at
// the top of this file — vendor strings legitimately survive in comments and
// provider notes, and none of those reach the model as the agent's Role.
var vendorIdentityMarkers = []string{
	"Nous Research",
	"self-improving",
	"hermes-agent",
	"Hermes Agent",
}

// assertNoVendorMarker fails if any vendor marker appears in a RENDERED identity
// field.
func assertNoVendorMarker(t *testing.T, field, value string) {
	t.Helper()
	for _, marker := range vendorIdentityMarkers {
		if strings.Contains(value, marker) {
			t.Errorf("composed config's rendered %s carries vendor identity %q — the runtime renders it as the agent's Role, where it competes with the concierge persona\n%s = %q",
				field, marker, field, value)
		}
	}
}

// TestComposeConciergeRuntimeConfig_OverridesName is the primary regression
// guard: the composed config's name is the ORG CONCIERGE's name — on BOTH
// compose exits, for every runtime.
func TestComposeConciergeRuntimeConfig_OverridesName(t *testing.T) {
	const conciergeName = "Acme Rockets Agent"
	for _, mode := range conciergeDeploymentModes {
		t.Run(mode.name, func(t *testing.T) {
			h := conciergeIdentityFixtureHandler(t, mode.orgID)
			for _, runtime := range []string{"hermes", "openclaw", "claude-code"} {
				t.Run(runtime, func(t *testing.T) {
					composed, err := h.composeConciergeRuntimeConfig(runtime, conciergeName)
					if err != nil {
						t.Fatalf("composeConciergeRuntimeConfig(%q) error: %v", runtime, err)
					}
					assertComposeExit(t, composed, mode.wantSchedules)
					gotName, _ := parseComposedIdentity(t, composed)
					if gotName != conciergeName {
						t.Errorf("composed name = %q, want %q\n%s", gotName, conciergeName, composed)
					}
				})
			}
		})
	}
}

// TestComposeConciergeRuntimeConfig_RenderedIdentityCarriesNoVendorMarker proves
// the competing identity is gone from the fields the runtime RENDERS — `name`
// and `description` — on both compose exits.
//
// It deliberately does NOT assert whole-document absence. `description` is the
// field that actually reached the model as Role and produced the incident; the
// vendor's name also survives in the real template's comments and provider notes,
// which are never rendered. See the scope note at the top of this file.
func TestComposeConciergeRuntimeConfig_RenderedIdentityCarriesNoVendorMarker(t *testing.T) {
	for _, mode := range conciergeDeploymentModes {
		t.Run(mode.name, func(t *testing.T) {
			h := conciergeIdentityFixtureHandler(t, mode.orgID)

			composed, err := h.composeConciergeRuntimeConfig("hermes", "Acme Rockets Agent")
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
			}
			assertComposeExit(t, composed, mode.wantSchedules)

			gotName, gotDesc := parseComposedIdentity(t, composed)
			assertNoVendorMarker(t, "name", gotName)
			assertNoVendorMarker(t, "description", gotDesc)

			if strings.TrimSpace(gotDesc) == "" {
				t.Error("composed description is empty; the concierge should describe its own role, not carry nothing")
			}
			if gotDesc != conciergeRoleDescription {
				t.Errorf("composed description is not the org-concierge role description\n got: %q\nwant: %q", gotDesc, conciergeRoleDescription)
			}
			if !strings.Contains(strings.ToLower(gotDesc), "concierge") {
				t.Errorf("composed description should describe the org-concierge role, got %q", gotDesc)
			}
		})
	}
}

// TestComposeConciergeRuntimeConfig_FixtureIsRepresentative is the ANTI-VACUOUS
// control. If the base fixture did not actually contain the vendor identity, the
// marker assertions would pass while measuring nothing. This asserts the fixture
// carries every marker in its RENDERED fields — the same fields the other tests
// then require to be clean.
func TestComposeConciergeRuntimeConfig_FixtureIsRepresentative(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	raw, err := os.ReadFile(filepath.Join(dir, "hermes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	baseName, baseDesc := parseComposedIdentity(t, raw)
	rendered := baseName + "\n" + baseDesc
	for _, marker := range vendorIdentityMarkers {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("base fixture's rendered name/description lack %q — the vendor-marker tests would pass vacuously\nname=%q\ndescription=%q",
				marker, baseName, baseDesc)
		}
	}
	// The platform-agent fixture must carry schedules, or the self-host matrix
	// cases silently degrade to the ungrafted path.
	pa, err := os.ReadFile(filepath.Join(dir, "platform-agent", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pa), "schedules:") {
		t.Fatal("platform-agent fixture has no schedules node — the schedules-grafted compose exit would never be exercised")
	}
}

// TestComposeConciergeRuntimeConfig_EmptyNameFallsBackToConciergeFallbackName
// guards the degenerate input.
//
// It asserts the fallback VALUE, not merely that the name is non-empty. The
// weaker assertion was satisfied by the exact incident signature: reinstating the
// BASE TEMPLATE's name ("Hermes Agent") leaves a non-empty name, and the vendor
// description is overridden regardless — so a "name is not blank" check goes
// GREEN on a config that says `name: Hermes Agent`.
func TestComposeConciergeRuntimeConfig_EmptyNameFallsBackToConciergeFallbackName(t *testing.T) {
	for _, mode := range conciergeDeploymentModes {
		t.Run(mode.name, func(t *testing.T) {
			h := conciergeIdentityFixtureHandler(t, mode.orgID)

			composed, err := h.composeConciergeRuntimeConfig("hermes", "")
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
			}
			assertComposeExit(t, composed, mode.wantSchedules)

			gotName, gotDesc := parseComposedIdentity(t, composed)
			if gotName != conciergeFallbackName {
				t.Errorf("empty concierge name produced name %q, want the fallback %q — an unknown name is never a reason to keep the base template's vendor name\n%s",
					gotName, conciergeFallbackName, composed)
			}
			assertNoVendorMarker(t, "name", gotName)
			assertNoVendorMarker(t, "description", gotDesc)
			if gotDesc != conciergeRoleDescription {
				t.Errorf("empty-name path description = %q, want %q", gotDesc, conciergeRoleDescription)
			}
			var probe map[string]interface{}
			if err := yaml.Unmarshal(composed, &probe); err != nil {
				t.Fatalf("composed config does not re-parse: %v", err)
			}
		})
	}
}

// TestComposeConciergeRuntimeConfig_IdentitySurvivesSchedulesGraft is the
// dedicated guard for composeConciergeRuntimeConfig's SECOND exit.
//
// The function returns either `out` (the first yaml.Marshal) or `withSched` from
// graftConciergeSchedules, which RE-MARSHALS the document. Those are two distinct
// serializations. Every self-host concierge takes the grafted one, so an identity
// override that lives only in the first — applied to a copy of the node tree, or
// to the marshaled bytes — would ship the vendor identity to exactly the
// deployments that run it.
//
// The schedules assertion is the anti-vacuous control: it proves the re-marshal
// actually happened, so the identity assertions below it are measured on the
// grafted bytes and not on a silently ungrafted document.
func TestComposeConciergeRuntimeConfig_IdentitySurvivesSchedulesGraft(t *testing.T) {
	h := conciergeIdentityFixtureHandler(t, "") // self-host: the graft is enabled.

	const conciergeName = "Acme Rockets Agent"
	for _, runtime := range []string{"hermes", "openclaw", "claude-code"} {
		t.Run(runtime, func(t *testing.T) {
			composed, err := h.composeConciergeRuntimeConfig(runtime, conciergeName)
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig(%q) error: %v", runtime, err)
			}
			names := parseComposedScheduleNames(t, composed)
			if len(names) != 1 || names[0] != "identity-daily-brief" {
				t.Fatalf("schedules = %v, want [identity-daily-brief] — the grafted exit did NOT run, so this test is not measuring it\n%s", names, composed)
			}
			gotName, gotDesc := parseComposedIdentity(t, composed)
			if gotName != conciergeName {
				t.Errorf("identity lost across the schedules re-marshal: name = %q, want %q\n%s", gotName, conciergeName, composed)
			}
			if gotDesc != conciergeRoleDescription {
				t.Errorf("identity lost across the schedules re-marshal: description = %q, want %q\n%s", gotDesc, conciergeRoleDescription, composed)
			}
			assertNoVendorMarker(t, "name", gotName)
			assertNoVendorMarker(t, "description", gotDesc)
		})
	}
}

// TestComposeConciergeRuntimeConfig_IdentityOverrideKeepsRuntimeContract proves
// the identity override did not regress the #2027 contract this function already
// carries: the composed config must still declare the ACTUAL runtime and still
// neutralize required_env.
func TestComposeConciergeRuntimeConfig_IdentityOverrideKeepsRuntimeContract(t *testing.T) {
	for _, mode := range conciergeDeploymentModes {
		t.Run(mode.name, func(t *testing.T) {
			h := conciergeIdentityFixtureHandler(t, mode.orgID)

			composed, err := h.composeConciergeRuntimeConfig("hermes", "Acme Rockets Agent")
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
			}
			assertComposeExit(t, composed, mode.wantSchedules)
			if got := parseTopLevelRuntime(composed); got != "hermes" {
				t.Errorf("composed runtime = %q, want hermes\n%s", got, composed)
			}
			if env := parseComposedRequiredEnv(t, composed); len(env) != 0 {
				t.Errorf("required_env = %v, want neutralized to []", env)
			}
			if pf := parseComposedPromptFiles(t, composed); len(pf) != 1 || pf[0] != conciergePersonaPromptPath {
				t.Errorf("prompt_files = %v, want [%s]", pf, conciergePersonaPromptPath)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The REAL shipped template.
// ---------------------------------------------------------------------------

// findRealWorkspaceConfigTemplates locates the SHIPPED workspace-configs-templates
// tree. It is gitignored in molecule-core (it is a separately-cloned template
// repo), so it is present on a developer/CI box that has cloned it and absent
// otherwise — hence the search rather than a hardcoded path.
//
// Order: MOLECULE_WORKSPACE_TEMPLATES_DIR, then each ancestor of the working
// directory. At each ancestor the REPO-SCOPED checkout
// (<ancestor>/molecule-core/workspace-configs-templates) is preferred over a bare
// <ancestor>/workspace-configs-templates, because the repo-scoped one is what the
// build actually bundles; a loose sibling clone next to it can be stale. Running
// from molecule-core itself, the repo's own copy is found first anyway (it sits at
// <repo>/workspace-configs-templates, an earlier ancestor). Returns "" when
// nothing is found.
func findRealWorkspaceConfigTemplates(t *testing.T) string {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv("MOLECULE_WORKSPACE_TEMPLATES_DIR")); p != "" {
		if _, err := os.Stat(filepath.Join(p, "hermes", "config.yaml")); err == nil {
			return p
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		for _, cand := range []string{
			filepath.Join(dir, "molecule-core", "workspace-configs-templates"),
			filepath.Join(dir, "workspace-configs-templates"),
		} {
			if _, err := os.Stat(filepath.Join(cand, "hermes", "config.yaml")); err == nil {
				return cand
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// TestComposeConciergeRuntimeConfig_RealTemplateRenderedIdentityIsClean drives the
// compose against the REAL shipped hermes template — 477 lines of provider
// registry, comments and model display names — instead of a 10-line fixture.
//
// It exists because a synthetic fixture cannot tell you the SHAPE of the
// guarantee. Measured here: the composed real-template output still contains
// "Nous Research" and "hermes-agent" in comments, `notes:` strings and model
// display names, preserved by the yaml.v3 Node round-trip. None of those is
// rendered as the agent's Role. So the honest, product-true invariant is exactly
// this: the RENDERED fields are clean. The counts are logged so the real scope is
// visible in the test output rather than assumed.
//
// SKIP, NOT PASS: when the template tree is absent this t.Skip()s, which Go
// reports as SKIP (never PASS). Set MOLECULE_REQUIRE_REAL_TEMPLATES=1 to turn the
// skip into a FAILURE where the tree is expected to exist.
func TestComposeConciergeRuntimeConfig_RealTemplateRenderedIdentityIsClean(t *testing.T) {
	root := findRealWorkspaceConfigTemplates(t)
	if root == "" {
		msg := "REAL-TEMPLATE INVARIANT NOT MEASURED: workspace-configs-templates not found (it is gitignored — clone it, or set MOLECULE_WORKSPACE_TEMPLATES_DIR). This run proves NOTHING about the shipped hermes template."
		if os.Getenv("MOLECULE_REQUIRE_REAL_TEMPLATES") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	t.Logf("real template tree: %s", root)

	raw, err := os.ReadFile(filepath.Join(root, "hermes", "config.yaml"))
	if err != nil {
		t.Fatalf("read real hermes config: %v", err)
	}
	// ANTI-VACUOUS: the shipped template must actually carry the vendor identity
	// in its rendered fields, or this test measures nothing.
	baseName, baseDesc := parseComposedIdentity(t, raw)
	if !strings.Contains(baseName, "Hermes Agent") {
		t.Fatalf("real hermes template name = %q — expected the vendor name; this test would be vacuous", baseName)
	}
	if !strings.Contains(baseDesc, "Nous Research") {
		t.Fatalf("real hermes template description = %q — expected the vendor blurb; this test would be vacuous", baseDesc)
	}

	for _, mode := range conciergeDeploymentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("MOLECULE_ORG_ID", mode.orgID)
			h := &WorkspaceHandler{configsDir: root}

			composed, err := h.composeConciergeRuntimeConfig("hermes", "Acme Rockets Agent")
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig on the real template: %v", err)
			}
			gotName, gotDesc := parseComposedIdentity(t, composed)
			if gotName != "Acme Rockets Agent" {
				t.Errorf("real-template composed name = %q, want %q", gotName, "Acme Rockets Agent")
			}
			if gotDesc != conciergeRoleDescription {
				t.Errorf("real-template composed description = %q, want the org-concierge role description", gotDesc)
			}
			assertNoVendorMarker(t, "name", gotName)
			assertNoVendorMarker(t, "description", gotDesc)

			// Which exit ran, on the REAL template (its platform-agent config may
			// or may not carry schedules — the hard exit assertion lives on the
			// fixture matrix, which controls that input).
			t.Logf("compose exit: schedules=%v", parseComposedScheduleNames(t, composed))

			// Measure — do not assert — the whole-document residue, so the true
			// scope of the guarantee is in the record. A whole-document assertion
			// here would be FALSE; see the scope note at the top of this file.
			for _, marker := range vendorIdentityMarkers {
				t.Logf("whole-document residue (NOT rendered as Role): %q x%d",
					marker, strings.Count(string(composed), marker))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The compose-ERROR fallback path.
// ---------------------------------------------------------------------------

// conciergeVendorDeliveredConfig is a DELIVERED config.yaml shaped like the
// runtime base template the asset channel actually ships — vendor identity
// included — plus a sentinel key. The sentinel is how the tests below prove they
// are on the compose-ERROR fallback and not silently measuring a freshly composed
// document: nothing but the delivered bytes can carry it.
const conciergeVendorDeliveredConfig = "name: Hermes Agent\n" +
	"description: >-\n" +
	"    Nous Research hermes-agent — the self-improving AI agent with built-in\n" +
	"    terminal, file ops, web search, memory, skills, and cross-session recall.\n" +
	"runtime: hermes\n" +
	"delivered_only_sentinel: asset-channel\n" +
	"runtime_config:\n" +
	"  model: minimax/MiniMax-M2.7\n"

// expectConciergeProvisionQueries wires the sqlmock expectations
// applyConciergeProvisionConfig issues for a kind=platform workspace whose model
// and provider are already pinned (so the reconciles are no-ops).
func expectConciergeProvisionQueries(t *testing.T, workspaceID, runtime, model string) sqlmock.Sqlmock {
	t.Helper()
	setConciergeModelResolver(t, model, nil)
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", runtime))
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).AddRow([]byte(model), 0))
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).AddRow([]byte("platform"), 0))
	mock.ExpectQuery(`SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs(workspaceID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	return mock
}

// TestApplyConciergeProvisionConfig_ComposeErrorStillOverridesVendorIdentity is
// the regression guard for the DEGRADED path.
//
// When resolveWorkspaceTemplatePath misses — template cache unpopulated after a
// fleet restart, or an image shipped without the template tree —
// composeConciergeRuntimeConfig returns an error and the hook falls back to the
// DELIVERED config. That delivered config is routinely the runtime base template
// itself, vendor `name`/`description` and all, so falling through unchanged
// reproduced the incident verbatim on exactly the path the fallback documents.
//
// The identity override must therefore also apply here.
func TestApplyConciergeProvisionConfig_ComposeErrorStillOverridesVendorIdentity(t *testing.T) {
	// EMPTY configs dir → resolveWorkspaceTemplatePath/ReadFile miss → compose errors.
	h := &WorkspaceHandler{configsDir: t.TempDir()}
	mock := expectConciergeProvisionQueries(t, "ws-degraded", "hermes", "minimax/MiniMax-M2.7")

	cf := map[string][]byte{
		"config.yaml":      []byte(conciergeVendorDeliveredConfig),
		"system-prompt.md": []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n"),
	}
	out := h.applyConciergeProvisionConfig(context.Background(), "ws-degraded", "", cf, map[string]string{}, "Acme Rockets Agent")

	got := out["config.yaml"]
	if len(got) == 0 {
		t.Fatal("no config.yaml delivered")
	}
	// ANTI-VACUOUS: prove we are on the FALLBACK path. Only the delivered bytes
	// carry the sentinel; a composed config could not.
	if !strings.Contains(string(got), "delivered_only_sentinel") {
		t.Fatalf("the sentinel is gone — this test is NOT exercising the compose-error fallback\n%s", got)
	}
	gotName, gotDesc := parseComposedIdentity(t, got)
	if gotName != "Acme Rockets Agent" {
		t.Errorf("fallback path shipped name = %q, want %q — the concierge boots with the VENDOR identity\n%s", gotName, "Acme Rockets Agent", got)
	}
	if gotDesc != conciergeRoleDescription {
		t.Errorf("fallback path shipped description = %q, want the org-concierge role description\n%s", gotDesc, got)
	}
	assertNoVendorMarker(t, "name", gotName)
	assertNoVendorMarker(t, "description", gotDesc)
	// The rest of the delivered config must survive intact (boot-safety).
	if rt := parseTopLevelRuntime(got); rt != "hermes" {
		t.Errorf("fallback path damaged the delivered config — runtime = %q, want hermes\n%s", rt, got)
	}
	// The historical {{CONCIERGE_NAME}} substitution must NOT regress.
	if sp := string(out["system-prompt.md"]); !strings.Contains(sp, "Acme Rockets Agent") || strings.Contains(sp, conciergeNamePlaceholder) {
		t.Errorf("fallback path lost the system-prompt substitution:\n%s", sp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyConciergeProvisionConfig_ComposeErrorUnparseableConfigShipsUnchanged
// is the BOOT-SAFETY control for the fix above. An unloadable config.yaml bricks
// workspace boot, so when the delivered config cannot be parsed the override must
// bail and ship the delivered bytes BYTE-IDENTICAL rather than emit a
// best-effort document. Same rule graftConciergeSchedules and
// stripConciergePersonaGraft already apply.
func TestApplyConciergeProvisionConfig_ComposeErrorUnparseableConfigShipsUnchanged(t *testing.T) {
	h := &WorkspaceHandler{configsDir: t.TempDir()}
	mock := expectConciergeProvisionQueries(t, "ws-broken", "hermes", "minimax/MiniMax-M2.7")

	// Unbalanced flow mapping → the whole document fails to parse.
	malformed := []byte("name: Hermes Agent\nproviders: [ {name: nous, models: ['x'\n")
	// Sanity: this really is unparseable, or the test proves nothing.
	var probe map[string]interface{}
	if err := yaml.Unmarshal(malformed, &probe); err == nil {
		t.Fatal("fixture parses cleanly — it cannot exercise the boot-safety bail")
	}
	cf := map[string][]byte{
		"config.yaml":      malformed,
		"system-prompt.md": []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n"),
	}
	out := h.applyConciergeProvisionConfig(context.Background(), "ws-broken", "", cf, map[string]string{}, "Acme Rockets Agent")

	if string(out["config.yaml"]) != string(malformed) {
		t.Errorf("unparseable delivered config was rewritten; it must ship UNCHANGED\n got: %s\nwant: %s", out["config.yaml"], malformed)
	}
	// The rest of the fallback still runs.
	if sp := string(out["system-prompt.md"]); !strings.Contains(sp, "Acme Rockets Agent") {
		t.Errorf("substitution skipped after the identity-override bail:\n%s", sp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyConciergeProvisionConfig_EmptyNameAgreesBetweenConfigAndPersona is the
// guard for the SHARED fallback.
//
// applyConciergeProvisionConfig passes one name to the compose AND to
// substituteConciergeName. The empty-name fallback used to live only inside
// compose, so an empty name — reachable on restart paths that do not hydrate
// payload.Name from the workspace row — produced `name: Org Concierge` in
// config.yaml and `# You are  — the Org Concierge` in the persona: the agent card
// and the system prompt disagreeing about who the agent is. That is the SAME
// defect class (two identities, one prompt) this change exists to remove.
func TestApplyConciergeProvisionConfig_EmptyNameAgreesBetweenConfigAndPersona(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-saas-xyz")
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}
	mock := expectConciergeProvisionQueries(t, "ws-noname", "hermes", "minimax/MiniMax-M2.7")

	out := h.applyConciergeProvisionConfig(context.Background(), "ws-noname", "", nil, map[string]string{}, "")

	gotName, _ := parseComposedIdentity(t, out["config.yaml"])
	if gotName != conciergeFallbackName {
		t.Errorf("config.yaml name = %q, want the fallback %q", gotName, conciergeFallbackName)
	}
	persona := string(out[conciergePersonaPromptPath])
	if persona == "" {
		t.Fatal("no persona delivered")
	}
	if strings.Contains(persona, conciergeNamePlaceholder) {
		t.Errorf("{{CONCIERGE_NAME}} survived in the persona:\n%s", persona)
	}
	// THE INVARIANT: the persona names the SAME agent the config card does.
	if !strings.Contains(persona, gotName) {
		t.Errorf("persona and config.yaml disagree about the agent's identity — config says %q, persona is:\n%s", gotName, persona)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
