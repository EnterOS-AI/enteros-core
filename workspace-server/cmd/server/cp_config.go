package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cpurl"
)

// refreshEnvFromCP pulls the tenant's current config-plane env vars
// from the control plane and applies them via os.Setenv BEFORE any
// other code calls os.Getenv on them.
//
// Why:
//   - user-data on the tenant EC2 bakes env vars into `docker run` at
//     provision time. Those values are frozen. When we rotate a secret
//     on CP (e.g. PROVISION_SHARED_SECRET) there's no way to push the
//     new value into already-provisioned tenants.
//   - the Docker image auto-updater already pulls the latest workspace-
//     server image every 5 min. If THAT image knows how to refresh its
//     own env from the CP on startup, every tenant heals itself within
//     the update cycle — no ssh, no re-provision, no ops toil.
//
// Contract (paired with cp-side GET /cp/tenants/config):
//
//	Request:  GET {MOLECULE_CP_URL or https://api.moleculesai.app}/cp/tenants/config
//	          Authorization: Bearer <ADMIN_TOKEN>
//	          X-Molecule-Org-Id: <MOLECULE_ORG_ID>
//	Response: 200 {"MOLECULE_CP_SHARED_SECRET":"…","MOLECULE_CP_URL":"…", …}
//	          401 on bearer mismatch or unknown org
//
// Best-effort: any failure logs and returns — main() keeps booting.
// Self-hosted deploys without MOLECULE_ORG_ID or ADMIN_TOKEN set
// short-circuit silently so this function is a no-op there.
func refreshEnvFromCP() error {
	orgID := os.Getenv("MOLECULE_ORG_ID")
	adminToken := os.Getenv("ADMIN_TOKEN")
	if orgID == "" || adminToken == "" {
		// Not a SaaS tenant (self-hosted dev or not yet provisioned).
		return nil
	}

	// Resolve via the single CP-URL seam (internal/cpurl). Behavior is
	// unchanged for managed tenants: MOLECULE_CP_URL, else the managed
	// default — for any tenant that lost track of its CP URL (e.g. older
	// user-data that only set MOLECULE_ORG_ID). A self-host operator can
	// redirect the default via MOLECULE_CP_DEFAULT_URL without code edits.
	base := cpurl.Base()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/cp/tenants/config", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-Molecule-Org-Id", orgID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 64 KiB cap — the CP only returns small JSON blobs here. An
	// unbounded read would be weaponizable if a compromised upstream
	// ever echoed back a gigabyte.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 401 on first boot-after-restart is expected for tenants still
		// running under old user-data where admin_token on-disk hasn't
		// had its corresponding row seeded. Don't treat as fatal — just
		// log so operators can spot repeat offenders in logs.
		return fmt.Errorf("cp returned %d", resp.StatusCode)
	}

	var cfg map[string]string
	if err := json.Unmarshal(body, &cfg); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	// Apply only strings; reject oversized values defensively. An
	// operator-supplied config should never exceed 4 KiB per key —
	// workspace-server env vars are URLs, hex secrets, short identifiers.
	const maxValueBytes = 4 << 10
	applied := 0
	for k, v := range cfg {
		if k == "" || len(v) > maxValueBytes {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			log.Printf("CP env refresh: setenv %s: %v", k, err)
			continue
		}
		applied++
	}
	log.Printf("CP env refresh: applied %d values from %s/cp/tenants/config", applied, base)
	return nil
}

// requiredLLMEnvVars is the set of LLM proxy env vars a managed SaaS
// tenant must have populated after refreshEnvFromCP. cp#469 (tenant
// proxy-env delivery) — guaranteed CP-delivered creds reach the
// tenant process env on boot. Per Researcher Task #37 / Spec 2 and
// Task #46 (watch-fail-first test).
//
// Key set byte-matched against Researcher's verified emission in
// controlplane tenant_config.go:140-144 (Researcher REQUEST_CHANGES
// iterate body, 3987f59c). The four keys below ARE the LLM-proxy
// subset of the 8 CP-emitted keys; OPENAI_BASE_URL / OPENAI_API_KEY /
// ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY are out of scope for cp#469
// (different feature surfaces — direct-to-provider fallbacks, not
// the proxy). v2 fix: MOLECULE_LLM_USAGE_TOKEN, MOLECULE_LLM_USAGE_URL,
// MOLECULE_LLM_BASE_URL, MOLECULE_LLM_ANTHROPIC_BASE_URL — note the
// 4th key is namespaced MOLECULE_LLM_ANTHROPIC_BASE_URL, NOT bare
// ANTHROPIC_BASE_URL. Bare ANTHROPIC_BASE_URL is a separate CP-emitted
// key for direct-provider use, not the LLM proxy.
var requiredLLMEnvVars = []string{
	"MOLECULE_LLM_USAGE_TOKEN",
	"MOLECULE_LLM_USAGE_URL", // CRITICAL fix v2: was MOLECULE_LLM_URL in v1
	"MOLECULE_LLM_BASE_URL",
	"MOLECULE_LLM_ANTHROPIC_BASE_URL", // CRITICAL fix v3: was ANTHROPIC_BASE_URL in v2 (different key!)
}

// assertManagedTenantHasLLMEnv verifies that, when running as a
// managed SaaS tenant (MOLECULE_ORG_ID + ADMIN_TOKEN both set), all
// required LLM proxy env vars are populated after refreshEnvFromCP.
//
// Self-hosted (no orgID/adminToken) is exempt — dev must not be
// blocked here. Managed tenants with missing LLM keys fail with
// MISSING_CP_LLM_ENV so they do not silently boot with broken proxy
// creds. Caller in main.go decides whether to log and continue or
// log.Fatalf depending on deployment context.
func assertManagedTenantHasLLMEnv() error {
	if os.Getenv("MOLECULE_ORG_ID") == "" || os.Getenv("ADMIN_TOKEN") == "" {
		// Self-hosted dev / not yet provisioned — not a managed tenant.
		return nil
	}
	var missing []string
	for _, k := range requiredLLMEnvVars {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("MISSING_CP_LLM_ENV: required LLM proxy keys not set after refreshEnvFromCP: %v", missing)
	}
	return nil
}

// assertSaaSTenantHasAdminToken refuses boot for a Molecule-managed tenant that
// has no ADMIN_TOKEN (core#4485). Without ADMIN_TOKEN, middleware.AdminAuth's
// deprecated Tier-3 fallback (wsauth_middleware.go:268-280) accepts ANY live
// workspace token as org-admin — so a workspace self-surface token could manage
// the org / create-delete-restart sibling workspaces. It ALSO silently skips the
// managed-tenant LLM-env assertion, because assertManagedTenantHasLLMEnv and
// refreshEnvFromCP both no-op when ADMIN_TOKEN is unset. A managed tenant missing
// ADMIN_TOKEN is thus misclassified as self-host and boots doubly-broken.
//
// Gate on MOLECULE_ORG_ID (NOT isSaaSDeployment): the CP provisioner is selected
// on ORG_ID alone (see main.go) and defaults to Molecule's control plane, so
// ORG_ID-set == a Molecule-managed tenant regardless of MOLECULE_DEPLOY_MODE. A
// real self-host / local-dev never sets MOLECULE_ORG_ID (it would misprovision
// against Molecule's CP), so it is EXEMPT and keeps the deprecated single-trust-
// domain Tier-3 path. Correctly-provisioned managed tenants always bake
// ADMIN_TOKEN into `docker run`, so this never fires for them. The caller
// (main.go) fatals on a non-nil return; assert BEFORE ensureManagedTenantLLMEnv.
func assertSaaSTenantHasAdminToken() error {
	if strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")) == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("ADMIN_TOKEN")) == "" {
		return fmt.Errorf("ADMIN_TOKEN_REQUIRED_FOR_MANAGED_TENANT: a Molecule-managed tenant (MOLECULE_ORG_ID set) must set ADMIN_TOKEN — without it admin routes fall back to accepting any live workspace token as org-admin (core#4485)")
	}
	return nil
}

// cpConfigRetryWindow / cpConfigRetryInterval bound how long a freshly-
// provisioned managed tenant retries the CP config fetch before giving up.
// Package vars (not consts) so tests can shrink them. Interval is a short poll
// (the row commits within ms–seconds of the first miss). Window is overridable
// via MOLECULE_CP_CONFIG_RETRY_WINDOW (Go duration, e.g. "45s") for ops tuning.
var (
	cpConfigRetryWindow   = 90 * time.Second
	cpConfigRetryInterval = 1 * time.Second
)

// resolveCPConfigRetryWindow returns the retry window, honoring a valid
// MOLECULE_CP_CONFIG_RETRY_WINDOW override. Invalid or non-positive values are
// REJECTED and LOGGED (never silently applied) — a "0" or a unit-less typo must
// not silently disable the retry (which would reinstate the very row-commit-race
// fatal this exists to prevent). Reads into a local (no mutation of the shared
// package var, so repeat/concurrent calls are idempotent).
func resolveCPConfigRetryWindow() time.Duration {
	window := cpConfigRetryWindow
	v := os.Getenv("MOLECULE_CP_CONFIG_RETRY_WINDOW")
	if v == "" {
		return window
	}
	switch d, err := time.ParseDuration(v); {
	case err != nil:
		log.Printf("CP env refresh: ignoring invalid MOLECULE_CP_CONFIG_RETRY_WINDOW=%q (%v) — using default %s", v, err, window)
	case d <= 0:
		log.Printf("CP env refresh: ignoring non-positive MOLECULE_CP_CONFIG_RETRY_WINDOW=%q — using default %s", v, window)
	default:
		window = d
	}
	return window
}

// ensureManagedTenantLLMEnv fetches the CP-delivered env (refreshEnvFromCP) and,
// for a MANAGED SaaS tenant whose required LLM-proxy env is still missing,
// RETRIES the fetch — but ONLY while it keeps FAILING (401 / network), which is
// the org_instances row-commit race — until it succeeds or a bounded window
// elapses. The caller fatals on a non-nil return (cp#469 — a managed tenant must
// not boot with broken proxy creds).
//
// Why retry ONLY on fetch failure: on a FRESH provision the tenant can boot and
// call GET /cp/tenants/config BEFORE the CP commits the org_instances row that
// carries this tenant's admin_token — the token lookup 401s and no env is
// delivered. A slow backend (EC2) always has the row committed first; a fast one
// (local-docker) races and loses. Retrying lets the row commit, then the fetch
// succeeds. But a fetch that SUCCEEDS (200) yet still lacks the required LLM env
// is NOT the race — the CP is up and answered; it is deterministic CP-side drift
// (e.g. one key dropped) that retrying cannot fix. We fatal on that IMMEDIATELY
// with its cause rather than burn the window. A persistent fetch failure fatals
// loudly once the window elapses, carrying the real fetch error (401 / connection
// refused), not a generic MISSING_CP_LLM_ENV.
//
// Blocks boot up to the window on a persistent fetch failure (before the HTTP
// listener starts — the cp#469 fail-loud contract requires the LLM env present
// before serving). Non-managed / self-host tenants (no MOLECULE_ORG_ID or
// ADMIN_TOKEN) return nil on the first assertion and never enter the loop.
func ensureManagedTenantLLMEnv() error {
	ferr := refreshEnvFromCP()
	if ferr != nil {
		log.Printf("CP env refresh: %v", ferr)
	}
	if assertManagedTenantHasLLMEnv() == nil {
		return nil // fast path (self-host: the assertion is a no-op → nil)
	}
	if ferr == nil {
		// CP answered 200 but the required LLM env is still missing → deterministic
		// drift, NOT the row-commit race. Retrying cannot fix it; fatal now with
		// this cause (do not burn the window).
		return assertManagedTenantHasLLMEnv()
	}

	// Fetch failed on the first try — likely the row-commit race (401 until the CP
	// commits our row). Retry until it succeeds or the window elapses.
	window := resolveCPConfigRetryWindow()
	log.Printf("CP env refresh: first fetch failed (%v) — retrying up to %s (CP org_instances row-commit race)", ferr, window)
	deadline := time.Now().Add(window)
	lastErr := ferr
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := cpConfigRetryInterval
		if remaining < sleep {
			sleep = remaining // never overshoot the window (also handles window < interval)
		}
		time.Sleep(sleep)
		if err := refreshEnvFromCP(); err != nil {
			lastErr = err
			log.Printf("CP env refresh retry: %v", err)
			continue
		}
		// Fetch now succeeds — the row committed.
		if err := assertManagedTenantHasLLMEnv(); err == nil {
			log.Printf("CP env refresh: LLM env delivered on retry")
			return nil
		} else {
			// 200 but still incomplete → deterministic drift; stop retrying.
			return err
		}
	}
	// Window elapsed with the fetch never succeeding — fatal with the real cause.
	return fmt.Errorf("CP config fetch did not succeed within %s: %w", window, lastErr)
}

