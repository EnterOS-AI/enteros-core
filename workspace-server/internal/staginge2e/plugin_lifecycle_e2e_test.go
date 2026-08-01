//go:build staging_e2e

package staginge2e

// plugin_lifecycle_e2e_test.go — live, against-real-staging guard for the
// tenant plugin-install lifecycle that backs the canvas Plugins tab.
//
// Guards three regressions that were each invisible to the existing suites:
//
//   1. Registry is non-empty (the canvas "no registry available" /
//      "Registry returned 0 plugins" report): GET /plugins on the tenant
//      ws-server must list the plugins clone-manifest.sh populates at deploy
//      time. A 0-length registry here is the exact server-side state that
//      makes the install dialog look broken.
//
//   2. ListInstalled returns an installed plugin on a SaaS workspace
//      (guards CP #3125 — the EIC branch that fixed "[] readback after a
//      successful install" for every SaaS tenant): install a registry plugin
//      via POST /workspaces/:id/plugins, then GET /workspaces/:id/plugins must
//      include it. Without the EIC branch this read back [] forever.
//
//   3. The agent stays online after the install-triggered restart (guards
//      the #159 mgmt-MCP self-heal — a plugin install restarts the workspace,
//      and a broken online-gate would leave it stuck 'failed'/offline): the
//      workspace must return to online+routable AND serve A2A after the
//      install.
//
// Reuses the workspace_lifecycle_test.go harness wholesale (requireStagingEnv
// / adminCreateOrg / adminDeleteTenant / tenantAdminToken / tenantCreateWorkspace
// / waitForWorkspaceOnlineRoutable / waitForWorkspaceStatus / doTenantJSON /
// serveProbe / jsonField). NOTHING here re-implements org provisioning or
// teardown; the shared harness schedules an exact-slug admin DELETE before it
// waits for provisioning, retries transient lifecycle conflicts, and requires
// exact-slug absence before the E2E can pass.
//
// Guarded by the staging_e2e build tag + STAGING_E2E=1 (requireStagingEnv).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPluginInstallLifecycle_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := e2eSlug("plgn")
	t.Logf("plugin-lifecycle: slug=%s", slug)

	// --- Step 1: provision throwaway org + tenant (reused scaffolding) ---
	orgID := adminCreateOrg(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	token := tenantAdminToken(t, cfg, slug)
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)
	// /health is allowlisted and goes green before the app's real proxied routes
	// serve (controlplane#1012). Gate on the actual /plugins route the next
	// subtest asserts, to a stable streak, before reading it — otherwise a
	// transient boot-window 503 flakes registry_non_empty.
	waitForTenantRoute(t, host, "/plugins", token, orgID, 2, 3*time.Minute, "tenant /plugins ready")

	// --- Step 2: registry must be non-empty BEFORE we pick a plugin ----------
	// This is independent of any workspace — the registry is host-local
	// (clone-manifest.sh populated). We both assert it's non-empty (guard #1)
	// AND use the first runtime-agnostic entry as the plugin to install, so the
	// test never hardcodes a registry slug that could be renamed/removed.
	var pluginName string
	t.Run("registry_non_empty", func(t *testing.T) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/plugins", token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /plugins registry: HTTP %d: %s", hs, body)
		}
		reg := parsePluginList(t, body)
		if len(reg) == 0 {
			t.Fatalf("GET /plugins returned an EMPTY registry — the canvas install dialog would "+
				"show 'no registry available'. clone-manifest.sh must populate plugins/ at deploy. body=%s",
				truncate(body, 400))
		}
		pluginName = pickInstallablePlugin(reg)
		if pluginName == "" {
			t.Fatalf("registry has %d entries but none with a usable name: %s", len(reg), truncate(body, 400))
		}
		t.Logf("registry has %d plugins; will install %q", len(reg), pluginName)
	})
	if pluginName == "" {
		t.Fatal("no installable plugin established by registry_non_empty — dependent subtests cannot run")
	}

	// --- Step 3: create + boot a workspace ----------------------------------
	wsID := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("workspace created: %s", wsID)
	waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "initial boot")
	t.Logf("workspace %s online + routable", wsID)

	// --- Step 4: install → ListInstalled returns it → agent stays online -----
	t.Run("install_then_list_then_stay_online", func(t *testing.T) {
		// Install the registry plugin via the local:// source — the same path
		// the canvas "Install" button uses (handleInstall → local://<name>).
		installBody, _ := json.Marshal(map[string]string{"source": "local://" + pluginName})
		hs, body := doTenantJSON(t, "POST", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, string(installBody))
		if hs != http.StatusOK && hs != http.StatusCreated && hs != http.StatusAccepted {
			t.Fatalf("install %q: HTTP %d: %s", pluginName, hs, body)
		}
		t.Logf("install %q accepted (HTTP %d) — workspace will restart", pluginName, hs)

		// The install restarts the workspace. Wait for it to come back
		// online+routable. A stuck 'failed'/offline here is the #159 self-heal
		// regression (online-gate refusing a freshly-rebooted box).
		waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "post-install restart→online")

		// ListInstalled must now include the plugin (guard #3125 EIC branch).
		// The readback can race the restart, so poll until it appears.
		if !pollListInstalledContains(t, host, token, orgID, wsID, pluginName, 5*time.Minute) {
			_, listBody := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
			t.Fatalf("installed plugin %q never appeared in ListInstalled within 5m — "+
				"the SaaS EIC readback (#3125) regressed; last list=%s", pluginName, truncate(listBody, 600))
		}
		t.Logf("ListInstalled returned %q after install (EIC readback OK)", pluginName)

		// And the agent must actually SERVE — online-row + a live A2A reply.
		// The status/url row can lead the A2A listener by a few seconds after an
		// install-triggered restart, so use the same warmup-tolerant assertion as
		// the workspace lifecycle gate instead of a one-shot probe.
		assertServes(t, host, token, orgID, wsID, "post-plugin-install")
		t.Logf("agent served A2A after install — stayed online through the restart")

		// ...and it must STILL be up minutes later. Every assertion above is
		// one-shot and a workspace in a restart LOOP satisfies all of them in
		// the window between restarts — that is exactly how core#5011 reached a
		// customer with this gate green. See assertWorkspaceStaysUp.
		assertWorkspaceStaysUp(t, host, token, orgID, wsID, 3*time.Minute, "post-plugin-install stability")
	})

	// --- Step 5: the core#182 local-docker 502 regression guard ---------------
	// On a molecules-server (local-docker) tenant, the CP persists the mol-ws-*
	// CONTAINER NAME into workspaces.instance_id. Pre-fix the plugins handler
	// dispatched on `instance_id != ""` and pushed the plugin over the AWS-only
	// EIC SSH path, which — with no AWS creds — hangs 90-120s and returns 502,
	// staging NOTHING. This is the exact failure that blocked the Lark-channel
	// live test. The fix routes local-docker delivery through docker
	// CopyToContainer into the mol-ws-* container. This subtest installs the
	// real Lark plugin by pinned gitea:// SHA and asserts the install COMPLETES
	// fast (NOT 502/504, and well under the EIC timeout floor).
	t.Run("gitea_lark_install_completes_not_502", func(t *testing.T) {
		const larkSource = "gitea://molecule-ai/lark-channel-molecule#e02201357065452412dba9a3f8cb194996d2d86a"
		installBody, _ := json.Marshal(map[string]string{"source": larkSource})

		start := time.Now()
		hs, body := doTenantJSON(t, "POST", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, string(installBody))
		elapsed := time.Since(start)

		// The 502/504 + ~90-120s duration is the precise EIC-timeout signature.
		if hs == http.StatusBadGateway || hs == http.StatusGatewayTimeout {
			t.Fatalf("Lark install returned HTTP %d after %s — this is the retired-AWS-EIC "+
				"fallback firing on a local-docker tenant (core#182). Delivery must go via "+
				"docker CopyToContainer into the mol-ws-* container. body=%s", hs, elapsed, truncate(body, 400))
		}
		if hs != http.StatusOK && hs != http.StatusCreated && hs != http.StatusAccepted {
			t.Fatalf("Lark install %s: HTTP %d after %s: %s", larkSource, hs, elapsed, truncate(body, 400))
		}
		// Docker delivery is seconds; the EIC hang was 90-120s. A generous 80s
		// ceiling still cleanly separates "delivered via docker" from "rode EIC".
		if elapsed > 80*time.Second {
			t.Fatalf("Lark install took %s — that is the EIC-hang duration, not a docker "+
				"CopyToContainer (which is seconds). The AWS EIC path is still being hit "+
				"for a local-docker tenant (core#182).", elapsed)
		}
		t.Logf("Lark install completed in %s (HTTP %d) — docker delivery, NOT AWS EIC", elapsed, hs)

		// The install restarts the workspace; let it settle so later assertions
		// (and teardown) see a healthy box.
		waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "post-lark-install restart→online")

		// And the Lark plugin must read back as installed (the same EIC-readback
		// path guard #3125 exercises), proving it actually landed in the container.
		if !pollListInstalledContains(t, host, token, orgID, wsID, "lark-channel-molecule", 5*time.Minute) {
			_, listBody := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
			t.Fatalf("Lark plugin never appeared in ListInstalled within 5m after a 2xx install — "+
				"delivery did not actually stage it. last list=%s", truncate(listBody, 600))
		}
		t.Logf("Lark plugin present in ListInstalled — delivery genuinely staged the plugin")
	})

	// --- Step 6: control plugin (superpowers) also installs -------------------
	// A second, independent plugin proves the docker-delivery path is general,
	// not a Lark-specific fluke. Installed from the platform registry (local://).
	t.Run("control_superpowers_install", func(t *testing.T) {
		installBody, _ := json.Marshal(map[string]string{"source": "local://superpowers"})

		start := time.Now()
		hs, body := doTenantJSON(t, "POST", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, string(installBody))
		elapsed := time.Since(start)

		if hs == http.StatusBadGateway || hs == http.StatusGatewayTimeout {
			t.Fatalf("superpowers install returned HTTP %d after %s — the AWS-EIC fallback is "+
				"firing on a local-docker tenant (core#182). body=%s", hs, elapsed, truncate(body, 400))
		}
		if hs != http.StatusOK && hs != http.StatusCreated && hs != http.StatusAccepted {
			t.Fatalf("control superpowers install: HTTP %d after %s: %s", hs, elapsed, truncate(body, 400))
		}
		if elapsed > 80*time.Second {
			t.Fatalf("superpowers install took %s — EIC-hang duration, not docker delivery (core#182).", elapsed)
		}
		t.Logf("control superpowers install completed in %s (HTTP %d) — docker delivery", elapsed, hs)

		waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "post-superpowers-install restart→online")
		if !pollListInstalledContains(t, host, token, orgID, wsID, "superpowers", 5*time.Minute) {
			_, listBody := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
			t.Fatalf("control superpowers never appeared in ListInstalled within 5m; last list=%s", truncate(listBody, 600))
		}
		t.Logf("control superpowers present in ListInstalled — docker delivery path is general")
	})
}

// ─── helpers (plugin-lifecycle specific; lifecycle suite owns the shared ones) ──

// pluginListRow is the flat view of one /plugins or /workspaces/:id/plugins row.
type pluginListRow struct {
	Name    string
	Version string
}

func parsePluginList(t *testing.T, body string) []pluginListRow {
	t.Helper()
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("plugins body not a JSON array: %v (%s)", err, truncate(body, 300))
	}
	out := make([]pluginListRow, 0, len(raw))
	for _, m := range raw {
		out = append(out, pluginListRow{
			Name:    rawString(m["name"]),
			Version: rawString(m["version"]),
		})
	}
	return out
}

// pickInstallablePlugin returns the first registry entry with a non-empty name.
// (Runtime filtering is handled server-side; an unnamed entry is unusable.)
func pickInstallablePlugin(reg []pluginListRow) string {
	for _, p := range reg {
		if p.Name != "" {
			return p.Name
		}
	}
	return ""
}

// pollListInstalledContains polls GET /workspaces/:id/plugins until name appears
// or the timeout elapses. Returns true if found.
func pollListInstalledContains(t *testing.T, host, token, orgID, wsID, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
		if hs == http.StatusOK {
			for _, p := range parsePluginList(t, body) {
				if p.Name == name {
					return true
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	return false
}

// assertWorkspaceStaysUp fails when a workspace RESTARTS during the sample
// window, by watching `uptime_seconds` for a DECREASE.
//
// WHY THIS EXISTS — core#5011, a production incident CI could not have caught.
//
// A customer's coordinator restarted 24-32 times an hour and its agent never
// survived long enough to complete a turn. The control plane's install
// classifier compared a freshly cloned staged tree against the LIVE tree and
// counted runtime-generated files (a plugin daemon's `*.egg-info/` and
// `__pycache__/*.pyc`) as content. Those never exist in a clone, so the trees
// differed permanently, every reconcile classified `cold` and restarted the
// workspace, and every restart fired fireReconcileOnline, which reconciled
// again:
//
//	reconcile -> cold -> restart -> online -> reconcile -> cold -> ...
//
// Every existing e2e assertion PASSED against that box. They are all one-shot:
// provision, assert online, assert a tool answers, done. A workspace in a
// ~100-second restart loop satisfies every one of them in the window between
// restarts. The gate was blind not because it lacked a plugin test, but because
// nothing ever asked "is it STILL up a few minutes later".
//
// uptime_seconds is monotonic within one container lifetime (the heartbeat
// writes it every cycle) and resets to 0 on restart, so a decrease is a restart
// — no docker access or log scraping required, and it works against a remote
// SaaS tenant.
//
// NOT a flake risk: a healthy workspace never decreases uptime. If this ever
// reds, a workspace really did restart while nothing asked it to.
func assertWorkspaceStaysUp(t *testing.T, host, token, orgID, wsID string, window time.Duration, why string) {
	t.Helper()
	const interval = 15 * time.Second
	deadline := time.Now().Add(window)
	var prev int64 = -1
	samples, restarts := 0, 0
	for time.Now().Before(deadline) {
		u := "https://" + host + "/workspaces/" + wsID
		_, body := doTenantJSON(t, "GET", u, token, orgID, "")
		cur := topLevelInt(body, "uptime_seconds")
		if cur < 0 {
			t.Fatalf("[%s] workspace read has no uptime_seconds — this gate cannot "+
				"observe restarts without it; body=%s", why, truncate(body, 300))
		}
		if prev >= 0 && cur < prev {
			restarts++
			t.Errorf("[%s] workspace RESTARTED: uptime_seconds went %d -> %d. "+
				"Nothing asked it to restart, so something is re-provisioning it in a "+
				"loop (core#5011: the install classifier counting runtime-generated "+
				"files as content is one way this happens).", why, prev, cur)
		}
		prev = cur
		samples++
		time.Sleep(interval)
	}
	if samples < 3 {
		t.Fatalf("[%s] only %d uptime samples in %s — the window is too short to "+
			"prove stability, and a gate that cannot observe must not report success",
			why, samples, window)
	}
	if restarts == 0 {
		t.Logf("[%s] workspace stayed up across %d samples over %s (no uptime reset)",
			why, samples, window)
	}
}

// topLevelInt is topLevelString's numeric sibling; -1 when missing/not a number.
func topLevelInt(body, key string) int64 {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return -1
	}
	raw, ok := m[key]
	if !ok {
		return -1
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return -1
	}
	return n
}
