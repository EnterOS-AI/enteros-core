package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

// cpConfigRetryWindow / cpConfigRetryInterval bound how long a freshly-
// provisioned managed tenant retries the CP config fetch before giving up.
// Package vars (not consts) so tests can shrink the window. Overridable via
// MOLECULE_CP_CONFIG_RETRY_WINDOW (Go duration, e.g. "45s") for ops tuning.
var (
	cpConfigRetryWindow   = 90 * time.Second
	cpConfigRetryInterval = 3 * time.Second
)

// ensureManagedTenantLLMEnv fetches the CP-delivered env (refreshEnvFromCP) and,
// for a MANAGED SaaS tenant whose required LLM-proxy env is still missing
// afterward, retries the fetch for a bounded window before returning the final
// assertion verdict. The caller fatals on a non-nil return (cp#469 — a managed
// tenant must not boot with broken proxy creds).
//
// Why the retry: on a FRESH provision the tenant can boot and call
// GET /cp/tenants/config BEFORE the CP has committed the org_instances row that
// carries this tenant's admin_token — the CP's token lookup then 401s and the
// LLM env is never delivered. A slow backend (EC2, minutes to boot) always has
// the row committed first and never sees this; a fast backend (local-docker,
// seconds) races and loses. Retrying lets the row commit, then the fetch
// succeeds. A PERSISTENT miss (genuine misconfig) still fatals loudly once the
// window elapses — the fail-loud guarantee is preserved, only deferred.
//
// Non-managed / self-host tenants (no MOLECULE_ORG_ID or ADMIN_TOKEN) return nil
// on the first assertion and never enter the retry loop — byte-identical to the
// prior single-shot behavior.
func ensureManagedTenantLLMEnv() error {
	if v := os.Getenv("MOLECULE_CP_CONFIG_RETRY_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			cpConfigRetryWindow = d
		}
	}

	if err := refreshEnvFromCP(); err != nil {
		log.Printf("CP env refresh: %v (continuing with baked-in env)", err)
	}
	if err := assertManagedTenantHasLLMEnv(); err == nil {
		return nil
	}

	log.Printf("CP env refresh: managed-tenant LLM env not ready after first fetch — retrying up to %s (CP org_instances row-commit race)", cpConfigRetryWindow)
	deadline := time.Now().Add(cpConfigRetryWindow)
	for time.Now().Before(deadline) {
		time.Sleep(cpConfigRetryInterval)
		if err := refreshEnvFromCP(); err != nil {
			log.Printf("CP env refresh retry: %v", err)
			continue
		}
		if assertManagedTenantHasLLMEnv() == nil {
			log.Printf("CP env refresh: LLM env delivered on retry")
			return nil
		}
	}
	// Window elapsed — return the final verdict (fatal upstream if still missing).
	return assertManagedTenantHasLLMEnv()
}
