package handlers

// payload_plugins_first_boot_test.go — pins the ordering property fixed by
// PR #4541 (finding #4) and tracked by issue #4544.
//
// PROPERTY: a Create carrying CreateWorkspacePayload.Plugins seeds those
// sources into workspace_declared_plugins BEFORE the provision dispatch, so
// they are captured by buildProvisionerConfig → desiredPluginSources at
// dispatch time and appear in the FIRST-BOOT MOLECULE_DECLARED_PLUGINS — not
// only later via the post-online reconcile → docker-cp → mid-lifecycle
// restart. workspace.go seeds the payload plugins in the pre-dispatch block
// (seedTemplatePlugins) just above provisionWorkspaceAuto for exactly this
// reason.
//
// Two complementary tests pin it:
//
//  1. TestPayloadPlugins_PreDispatchSeed_LandInFirstBootDesiredSet — a
//     DB-state table test that runs the REAL seed (payloadPluginsToSeed →
//     seedTemplatePlugins → recordDeclaredPlugin's INSERT) and the REAL read
//     (desiredPluginSources → listDeclaredPlugins) against sqlmock, in the two
//     temporal orders. seed-BEFORE-read (the fixed, pre-dispatch shape) →
//     the payload source IS in the desired set the provisioner stamps as
//     MOLECULE_DECLARED_PLUGINS. read-BEFORE-seed (the pre-fix shape, where
//     the seed lands after provisionWorkspaceAuto) → the declared table is
//     still empty at dispatch, so the payload source is ABSENT from the
//     first-boot desired set. The absence in the second row is the intrinsic
//     negative control: it proves the assertion is ordering-sensitive, not a
//     tautology.
//
//  2. TestPayloadPluginSeed_PrecedesProvisionDispatch — an AST call-order gate
//     (sibling to scheduler_declare_before_provision_gate_test.go, reusing its
//     assertCallBefore helper) that pins the REAL workspace.go Create source:
//     the payload seed runs BEFORE the provisionWorkspaceAuto dispatch. It
//     anchors on payloadPluginsToSeed — the payload-specific dedup/cap call
//     that is unique to the pre-dispatch payload block. (seedTemplatePlugins is
//     NOT a valid anchor: Create also calls it for the TEMPLATE declared-plugin
//     paths AFTER the dispatch on purpose — those install via the post-online
//     reconcile, RFC#2843 #32. Only the payload plugins must be pre-dispatch to
//     reach the first-boot env, which is exactly finding #4.) Reachable fail
//     arm / negative control: move the payload seed block below
//     provisionWorkspaceAuto (the pre-#4541 shape) and this test fails.

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestPayloadPlugins_PreDispatchSeed_LandInFirstBootDesiredSet pins the state
// property: desiredPluginSources — the function buildProvisionerConfig reads at
// provision dispatch to assemble MOLECULE_DECLARED_PLUGINS — surfaces a payload
// plugin IF AND ONLY IF the seed ran before it (pre-dispatch). The two table
// rows exercise the identical real functions in the two orders; only the order
// differs, and only the fixed order yields the payload source at first boot.
func TestPayloadPlugins_PreDispatchSeed_LandInFirstBootDesiredSet(t *testing.T) {
	const (
		wsID       = "ws-payload-firstboot"
		payloadSrc = "gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.1.0"
		wantName   = "molecule-ai-plugin-digest-mail"
	)

	// Guard the fixtures: the seed derives the declared name from the source
	// via PluginNameFromSource, so wantName must match what the real derivation
	// produces or the INSERT WithArgs assertion below would be meaningless.
	if got, err := plugins.PluginNameFromSource(payloadSrc); err != nil || got != wantName {
		t.Fatalf("fixture drift: PluginNameFromSource(%q) = (%q, %v), want (%q, nil)", payloadSrc, got, err, wantName)
	}

	// declared is the result of the REAL payload dedup/cap pass; seed records it.
	declared, ok := payloadPluginsToSeed([]string{payloadSrc})
	if !ok || len(declared) != 1 || declared[0] != payloadSrc {
		t.Fatalf("payloadPluginsToSeed(%q) = (%v, %v), want ([%q], true)", payloadSrc, declared, ok, payloadSrc)
	}

	cases := []struct {
		name         string
		seedFirst    bool // true = pre-dispatch seed (fixed); false = post-dispatch seed (pre-fix)
		wantContains bool
	}{
		{
			name:         "pre-dispatch seed (fixed): payload lands in first-boot MOLECULE_DECLARED_PLUGINS",
			seedFirst:    true,
			wantContains: true,
		},
		{
			name:         "post-dispatch seed (pre-fix): payload MISSING from first-boot set — would arrive only via post-online reconcile",
			seedFirst:    false,
			wantContains: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := withMockDB(t)
			defer cleanup()
			ctx := context.Background()

			// programSeed programs the seed's INSERT into workspace_declared_plugins.
			// WithArgs pins that the REAL seed derived (wsID, wantName, payloadSrc).
			programSeed := func() {
				mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
					WithArgs(wsID, wantName, payloadSrc).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			// programRead programs the two queries desiredPluginSources runs. The
			// declared rows reflect the table state AT READ TIME: the seeded row if
			// the seed already ran, otherwise empty (nothing declared yet).
			programRead := func(declaredSeeded bool) {
				dRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
				if declaredSeeded {
					dRows.AddRow(wantName, payloadSrc)
				}
				mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_declared_plugins`).
					WithArgs(wsID).
					WillReturnRows(dRows)
				mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_plugins`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
			}

			// Program expectations AND invoke the real functions in the scenario's
			// order. sqlmock is ordered, so this also enforces the DB-op sequence.
			var got []string
			if tc.seedFirst {
				programSeed()
				programRead(true) // seed already landed → declared table has the row
				if recorded, skipped := seedTemplatePlugins(ctx, wsID, declared); recorded != 1 || skipped != 0 {
					t.Fatalf("seedTemplatePlugins recorded=%d skipped=%d, want 1/0", recorded, skipped)
				}
				var err error
				if got, err = desiredPluginSources(ctx, wsID); err != nil {
					t.Fatalf("desiredPluginSources: %v", err)
				}
			} else {
				programRead(false) // dispatch read fires first → declared table empty
				programSeed()      // seed lands after dispatch — too late for first boot
				var err error
				if got, err = desiredPluginSources(ctx, wsID); err != nil {
					t.Fatalf("desiredPluginSources: %v", err)
				}
				if recorded, skipped := seedTemplatePlugins(ctx, wsID, declared); recorded != 1 || skipped != 0 {
					t.Fatalf("seedTemplatePlugins recorded=%d skipped=%d, want 1/0", recorded, skipped)
				}
			}

			has := false
			for _, s := range got {
				if s == payloadSrc {
					has = true
					break
				}
			}
			if has != tc.wantContains {
				t.Errorf("first-boot desired set = %v; payload source %q present=%v, want present=%v",
					got, payloadSrc, has, tc.wantContains)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// TestPayloadPluginSeed_PrecedesProvisionDispatch pins the workspace Create
// source: the payload-plugin seed runs BEFORE the provisionWorkspaceAuto
// dispatch, so the entries land in workspace_declared_plugins before the
// provision goroutine's buildProvisionerConfig → desiredPluginSources reads
// them. Anchored on payloadPluginsToSeed, the payload-specific call unique to
// the pre-dispatch payload block (seedTemplatePlugins is deliberately reused by
// the post-dispatch template declared-plugin paths, so it is not a valid
// ordering anchor here). Reuses the assertCallBefore AST helper from
// scheduler_declare_before_provision_gate_test.go.
//
// Negative control: relocating the payload seed block below
// provisionWorkspaceAuto (the pre-#4541 shape) makes the before-offset exceed
// the after-offset and this test fails; deleting the payload seed trips the
// "no longer calls" fail arm.
func TestPayloadPluginSeed_PrecedesProvisionDispatch(t *testing.T) {
	assertCallBefore(t, "workspace.go", "Create", "payloadPluginsToSeed", "provisionWorkspaceAuto")
}
