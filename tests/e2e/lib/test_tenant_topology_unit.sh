#!/usr/bin/env bash
# Unit test for derive_tenant_topology (tenant_topology.sh). Pure logic, no
# network. Wired in .gitea/workflows/ci.yml alongside the other lib *_unit.sh.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=tenant_topology.sh
source "$SCRIPT_DIR/tenant_topology.sh" || { echo "FAIL: cannot source tenant_topology.sh" >&2; exit 1; }

PASS=0; FAIL=0
ok()   { echo "PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $1" >&2; FAIL=$((FAIL+1)); }
eq()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want='$3' got='$2')"; fi; }

# Reset all inputs + outputs between cases.
reset() {
  unset MOLECULE_TENANT_URL MOLECULE_TENANT_DOMAIN MOLECULE_TENANT_ROUTE_HOST \
        MOLECULE_TENANT_ROUTE_DOMAIN MOLECULE_TENANT_ROUTE_PORT MOLECULE_TENANT_ORIGIN_TEMPLATE
  TENANT_URL=""; TENANT_ROUTE_HOST=""; TENANT_ROUTE_HDRS=(); TENANT_ORIGIN=""
}
hdrs() { printf '%s' "${TENANT_ROUTE_HDRS[*]:-}"; }   # array → space-joined string

CP_STAGING="https://staging-api.example.com"
CP_EPHEM="http://controlplane:8080"

# ── 1. Staging default: all MOLECULE_* unset ⇒ exact staging behaviour ──
reset
derive_tenant_topology "myslug" "$CP_STAGING"; rc=$?
eq "staging default rc=0" "$rc" "0"
eq "staging TENANT_URL = slug.staging.<domain>" "$TENANT_URL" "https://myslug.staging.example.com"
eq "staging has NO route headers" "$(hdrs)" ""
eq "staging TENANT_ORIGIN = TENANT_URL" "$TENANT_ORIGIN" "https://myslug.staging.example.com"

# ── 2. Ephemeral slug-routing: CP base URL + route domain, no origin template ──
reset
MOLECULE_TENANT_URL="$CP_EPHEM"
MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me"
derive_tenant_topology "eph1" "$CP_EPHEM"; rc=$?
eq "ephemeral rc=0" "$rc" "0"
eq "ephemeral TENANT_URL = CP base" "$TENANT_URL" "http://controlplane:8080"
eq "ephemeral route headers = Host + org-slug" "$(hdrs)" "-H Host: eph1.lvh.me -H X-Molecule-Org-Slug: eph1"
# Origin is DERIVED (not the CP base URL, which would 403 via cors): scheme+port
# from TENANT_URL, host from the route host.
eq "ephemeral TENANT_ORIGIN derived from route host + TENANT_URL port" "$TENANT_ORIGIN" "http://eph1.lvh.me:8080"

# ── 3. Explicit origin template ALWAYS wins ──
reset
MOLECULE_TENANT_URL="$CP_EPHEM"
MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me"
MOLECULE_TENANT_ORIGIN_TEMPLATE="http://{slug}.lvh.me:8080"
derive_tenant_topology "eph2" "$CP_EPHEM"; rc=$?
eq "origin-template rc=0" "$rc" "0"
eq "origin-template substituted with slug" "$TENANT_ORIGIN" "http://eph2.lvh.me:8080"

# ── 4. Explicit MOLECULE_TENANT_ROUTE_HOST overrides the <slug>.<domain> derivation ──
reset
MOLECULE_TENANT_URL="$CP_EPHEM"
MOLECULE_TENANT_ROUTE_HOST="custom-host.lvh.me"
derive_tenant_topology "eph3" "$CP_EPHEM"; rc=$?
eq "explicit route host used verbatim" "$TENANT_ROUTE_HOST" "custom-host.lvh.me"
eq "explicit route host in headers" "$(hdrs)" "-H Host: custom-host.lvh.me -H X-Molecule-Org-Slug: eph3"

# ── 4b. PROD CP hostname (api.<domain>) derivation ──
reset
derive_tenant_topology "prod1" "https://api.moleculesai.app"
eq "prod api.* derives <domain> (drops the api. label)" "$TENANT_URL" "https://prod1.moleculesai.app"

# ── 5. Staging with explicit tenant-domain override ──
reset
MOLECULE_TENANT_DOMAIN="custom.example.io"
derive_tenant_topology "dom1" "$CP_STAGING"; rc=$?
eq "domain override" "$TENANT_URL" "https://dom1.custom.example.io"

# ── 6. NEGATIVE CONTROL: routing active but TENANT_URL has no usable scheme and
#      no origin template ⇒ cannot derive a CORS origin ⇒ hard error (rc=1). ──
reset
MOLECULE_TENANT_URL="noscheme-garbage"
MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me"
err=$(derive_tenant_topology "eph4" "$CP_EPHEM" 2>&1); rc=$?
eq "un-derivable origin fails closed rc=1" "$rc" "1"
if printf '%s' "$err" | grep -q "Cannot derive a tenant CORS Origin"; then
  ok "un-derivable origin emits the actionable error"
else
  bad "un-derivable origin error message"
fi

echo "=== tenant_topology unit: passed=$PASS failed=$FAIL ==="
[ "$FAIL" = "0" ]
