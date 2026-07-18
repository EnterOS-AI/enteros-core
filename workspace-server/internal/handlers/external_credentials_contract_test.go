package handlers

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The credential/privilege SSOT is molecule-ai-sdk contracts/credentials. Its own
// schema $comment has said since sdk#78 that "core/runtime/mcp-server vendor it and
// a drift gate fails CI on a name mismatch" — but in core that gate was never built.
// Nothing stopped an operator-facing snippet from teaching a credential env-key name
// the contract has never heard of, which is the exact class of bug the contract
// exists to prevent (the concierge AUTH_ERROR: three repos, three names for the org
// credential, nothing forcing agreement).
//
// This is that gate. It runs against the VENDORED copy so it is hermetic and offline;
// .gitea/workflows/e2e-external-connect-snippet.yml re-fetches the SDK's main and
// fails if the vendored copy has drifted, so the pin cannot silently go stale.

const vendoredCredentialsContract = "testdata/sdk-credentials.contract.json"

type credEntry struct {
	ID           string   `json:"id"`
	EnvKey       string   `json:"env_key"`
	BoxedEnvKey  string   `json:"boxed_env_key"`
	Aliases      []string `json:"aliases"`
	ForbiddenOn  []string `json:"forbidden_on"`
	DisclosureOf *struct {
		ShownOnce bool   `json:"shown_once"`
		Recovery  string `json:"recovery"`
	} `json:"disclosure"`
}

func TestProductionPromoteCredentialIsForbiddenFromTenantBoxes(t *testing.T) {
	c := loadCredentialsContract(t)
	var promote *credEntry
	for i := range c.Credentials {
		if c.Credentials[i].EnvKey == "CP_PROMOTE_PROD_API_TOKEN" {
			promote = &c.Credentials[i]
			break
		}
	}
	if promote == nil {
		t.Fatal("vendored SDK credentials contract is missing CP_PROMOTE_PROD_API_TOKEN")
	}
	forbidden := make(map[string]struct{}, len(promote.ForbiddenOn))
	for _, surface := range promote.ForbiddenOn {
		forbidden[surface] = struct{}{}
	}
	for _, surface := range []string{"ordinary-workspace", "tenant-platform-box", "tenant-concierge"} {
		if _, ok := forbidden[surface]; !ok {
			t.Errorf("promote credential must be forbidden on %q; got %v", surface, promote.ForbiddenOn)
		}
	}
	if !isForbiddenTenantEnvKey(promote.EnvKey) {
		t.Errorf("%s is not in the tenant forbidden-env guard", promote.EnvKey)
	}
	if got := findPrivilegedTenantAdminEnvKeys(map[string]string{promote.EnvKey: "sentinel"}); len(got) != 1 || got[0] != promote.EnvKey {
		t.Errorf("final tenant admin guard = %v, want [%s]", got, promote.EnvKey)
	}
}

type credentialsContract struct {
	Credentials []credEntry `json:"credentials"`
	Routing     []credEntry `json:"routing"`
	Disclosure  struct {
		UnavailableMarker string `json:"unavailable_marker"`
		GuardNeedle       string `json:"guard_needle"`
		GuardSentinel     string `json:"guard_sentinel"`
		Rules             []struct {
			ID string `json:"id"`
		} `json:"rules"`
	} `json:"disclosure"`
}

func loadCredentialsContract(t *testing.T) credentialsContract {
	t.Helper()
	b, err := os.ReadFile(vendoredCredentialsContract)
	if err != nil {
		t.Fatalf("read the vendored SDK credentials contract (%s): %v — re-vendor with:\n"+
			"  curl -fsS -A curl/8.4.0 https://git.moleculesai.app/molecule-ai/molecule-ai-sdk/raw/branch/main/contracts/credentials/credentials.contract.json \\\n"+
			"    -o workspace-server/internal/handlers/%s",
			vendoredCredentialsContract, err, vendoredCredentialsContract)
	}
	var c credentialsContract
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("the vendored SDK credentials contract is not valid JSON: %v", err)
	}
	return c
}

// declaredEnvKeys is every env-key name the contract knows — canonical keys and
// aliases, across credentials[] and routing[].
func declaredEnvKeys(c credentialsContract) map[string]string {
	out := map[string]string{}
	for _, group := range [][]credEntry{c.Credentials, c.Routing} {
		for _, e := range group {
			for _, k := range append([]string{e.EnvKey, e.BoxedEnvKey}, e.Aliases...) {
				if k != "" {
					if _, seen := out[k]; !seen {
						out[k] = e.ID
					}
				}
			}
		}
	}
	return out
}

// TestSnippetGuards_PinnedToSDKCredentialsContract — core's guard constants ARE the
// contract's, not a parallel invention.
//
// Without this, a well-meaning edit to tokenUnavailableMarker in core silently
// diverges from the SSOT that every other consumer pins to, and every guard here
// keeps passing while meaning something different from what the contract says.
func TestSnippetGuards_PinnedToSDKCredentialsContract(t *testing.T) {
	c := loadCredentialsContract(t)
	d := c.Disclosure

	if tokenUnavailableMarker != d.UnavailableMarker {
		t.Errorf("tokenUnavailableMarker = %q, but the SDK credentials contract declares "+
			"disclosure.unavailable_marker = %q. Core does not get to pick this string: it is the "+
			"SSOT's, and other consumers pin the same value.", tokenUnavailableMarker, d.UnavailableMarker)
	}
	if tokenGuardNeedle != d.GuardNeedle {
		t.Errorf("tokenGuardNeedle = %q, contract disclosure.guard_needle = %q", tokenGuardNeedle, d.GuardNeedle)
	}
	if tokenGuardSentinel != d.GuardSentinel {
		t.Errorf("tokenGuardSentinel = %q, contract disclosure.guard_sentinel = %q", tokenGuardSentinel, d.GuardSentinel)
	}

	// The contract's own invariant, re-asserted here so a bad re-vendor cannot import
	// a contract whose guards would be dead code in THIS repo.
	if !strings.Contains(d.UnavailableMarker, d.GuardNeedle) {
		t.Errorf("the vendored contract declares guard_needle %q which does not occur in "+
			"unavailable_marker %q — every guard in this repo matches on the needle and would "+
			"never fire. Do not vendor this.", d.GuardNeedle, d.UnavailableMarker)
	}

	// The workspace-token is the credential the snippets carry. If the contract ever
	// stops calling it shown_once, the whole guard apparatus here is unmotivated —
	// that should be a loud, deliberate change, not a silent one.
	var wt *credEntry
	for i := range c.Credentials {
		if c.Credentials[i].ID == "workspace-token" {
			wt = &c.Credentials[i]
		}
	}
	if wt == nil {
		t.Fatalf("the contract no longer declares a workspace-token credential")
	}
	if wt.DisclosureOf == nil || !wt.DisclosureOf.ShownOnce {
		t.Errorf("the contract no longer marks workspace-token as shown_once. Every refusal guard " +
			"in external_connection.go exists BECAUSE the plaintext is unrecoverable; if that has " +
			"genuinely changed, delete the guards deliberately rather than leaving them unexplained.")
	}
}

// placeholderAssignmentRe matches an env-key assignment whose value is EXACTLY one of
// the server-stamped placeholders — `KEY={{AUTH_TOKEN}}`, `export KEY="{{PLATFORM_URL}}"`,
// `KEY = "{{WORKSPACE_ID}}"` (codex TOML), `KEY: "{{...}}"` — in shell, TOML, and YAML.
//
// "Exactly" is the point. It deliberately does NOT match MOLECULE_WS_ENTRY='{"id":
// "{{WORKSPACE_ID}}", …}' (a JSON transport blob, not an env-key contract) or
// "MOLECULE_WORKSPACE_TOKEN": "$MOLECULE_WORKSPACE_TOKEN" (an indirection — the real
// assignment site is elsewhere and gets checked there). What it catches is precisely
// the thing that matters: a snippet TEACHING an operator to put a platform credential
// or routing value into an env var under some name. That name must be one the SSOT
// declares, or the operator's agent will read a variable nobody sets.
var placeholderAssignmentRe = regexp.MustCompile(
	`(?m)^[ \t]*(?:export[ \t]+)?([A-Z][A-Z0-9_]*)[ \t]*[=:][ \t]*"?(\{\{(?:AUTH_TOKEN|PLATFORM_URL|WORKSPACE_ID)\}\})"?[ \t]*\\?$`,
)

// TestSnippetCredentials_ConformToSDKCredentialsContract — every env-key an
// operator-facing snippet assigns a stamped platform value to must be DECLARED in the
// SDK credentials contract (as a canonical key or an accepted alias).
//
// This is the drift gate the contract's schema has promised since sdk#78. Building it
// immediately found two undeclared names in the shipped snippets:
//
//   - WORKSPACE_AUTH_TOKEN (curl snippet) — read by NOTHING anywhere in the platform;
//     it appeared only in credential-scrub allowlists. Renamed to the canonical
//     MOLECULE_WORKSPACE_TOKEN in this PR.
//   - MOLECULE_PLATFORM_URL (hermes snippet) — real, and read by the SDK's OWN connect
//     CLI, but the contract had never declared it. Declared as a tenant-url alias in
//     the SDK (sdk#97): the SSOT was incomplete, so the SSOT was fixed.
//
// Neither would have been caught by any test that existed.
func TestSnippetCredentials_ConformToSDKCredentialsContract(t *testing.T) {
	c := loadCredentialsContract(t)
	declared := declaredEnvKeys(c)

	// Render with the placeholders INTACT (not a stamped payload): the gate is about
	// the env-key NAMES the templates teach, which is a property of the template.
	var offenders []string
	for name, tmpl := range externalSnippetTemplates {
		if reason, exempt := snippetsWithoutEnvKeyContract[name]; exempt {
			// Hold the exemption to its own premise. It claims the credential never
			// travels as an env var in this snippet — so the snippet must not read it
			// out of the environment either. If it does, the taxonomy DOES apply and
			// the exemption is hiding real drift.
			for _, envRead := range []string{"os.environ", "os.getenv", "getenv(", "process.env"} {
				if strings.Contains(tmpl, envRead) {
					t.Errorf("%s is listed in snippetsWithoutEnvKeyContract (%q) but contains %q — "+
						"it DOES move the credential through the environment, so the contract's "+
						"env-key taxonomy applies to it. Drop the exemption.", name, reason, envRead)
				}
			}
			continue
		}
		seen := map[string]bool{}
		for _, m := range placeholderAssignmentRe.FindAllStringSubmatch(tmpl, -1) {
			key := m[1]
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, ok := declared[key]; !ok {
				offenders = append(offenders, name+": "+key+" = "+m[2])
			}
		}
	}
	sort.Strings(offenders)

	// A stale exemption silently exempts nothing today and the WRONG thing after a rename.
	for name := range snippetsWithoutEnvKeyContract {
		if _, ok := externalSnippetTemplates[name]; !ok {
			t.Errorf("snippetsWithoutEnvKeyContract names %q, which is not a registered snippet", name)
		}
	}

	if len(offenders) > 0 {
		keys := make([]string, 0, len(declared))
		for k := range declared {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("these operator-facing snippets assign a stamped platform value to an env key the "+
			"SDK credentials contract does NOT declare:\n\n  %s\n\n"+
			"An undeclared credential/routing env-key name is the exact bug the credentials contract "+
			"exists to prevent — the concierge AUTH_ERROR was three repos using three names for one "+
			"credential with nothing forcing agreement. Either use a declared name, or (if the name is "+
			"real and something genuinely reads it) declare it in the SDK contract FIRST and re-vendor "+
			"testdata/sdk-credentials.contract.json. Do not silently invent a name here.\n\n"+
			"Declared names: %s",
			strings.Join(offenders, "\n  "), strings.Join(keys, ", "))
	}
}

// TestSnippetCredentials_TokenSitesUseTheCanonicalName goes one step past "declared":
// the workspace credential must be taught under its CANONICAL name, not an alias.
//
// Aliases exist so old boxes keep working; new operator-facing material should not
// mint more of them. workspace-token declares aliases: [] anyway — so any name other
// than MOLECULE_WORKSPACE_TOKEN at a token site is, by construction, undeclared.
func TestSnippetCredentials_TokenSitesUseTheCanonicalName(t *testing.T) {
	c := loadCredentialsContract(t)
	var canonical string
	for _, e := range c.Credentials {
		if e.ID == "workspace-token" {
			canonical = e.EnvKey
		}
	}
	if canonical == "" {
		t.Fatalf("the contract declares no workspace-token env_key")
	}

	for name, tmpl := range externalSnippetTemplates {
		if _, exempt := snippetsWithoutEnvKeyContract[name]; exempt {
			continue // not an env var at all — see snippetsWithoutEnvKeyContract
		}
		for _, m := range placeholderAssignmentRe.FindAllStringSubmatch(tmpl, -1) {
			key, val := m[1], m[2]
			if val != "{{AUTH_TOKEN}}" {
				continue
			}
			if key != canonical {
				t.Errorf("%s assigns the workspace credential to %s, but the SDK contract's canonical "+
					"name is %s (aliases: none). An operator who follows this snippet exports a variable "+
					"the runtime never reads.", name, key, canonical)
			}
		}
	}
}
