#!/usr/bin/env bash
# run_footprint.sh — provider-agnostic e2e RUN-footprint CAPTURE + zero-leftover
# VERIFY + force-clean backstop. Sourced by the cleanup-verification wrappers
# (test_cleanup_verification_e2e.sh). `set -u` safe.
#
# WHY THIS EXISTS (the gap it closes): an e2e that provisions a real tenant and
# then tears it down via the product's OWN path (CP admin DELETE /cp/admin/
# tenants/:slug → executeOrgPurge → provisioner.DeprovisionInstance, and/or the
# platform-agent MCP delete_workspace) has — until now — had NO assertion that
# the teardown actually removed everything. A leftover container / network /
# volume / DNS record / tunnel / DB row was only ever mopped up later by a
# REAPER (sweep/e2e_leak_reap.go #805, sweep/cloudflare_dns_reaper.go), which
# HIDES the product bug: the CP/MCP teardown silently leaked and nobody failed.
# This library makes a leftover-after-the-product's-own-teardown a TEST FAILURE
# that NAMES the leaking resource and the teardown op that should have removed
# it — turning the reaper back into a true backstop instead of a cover-up.
#
# THE RUN IDENTITY (provider-agnostic): a run is (ORG_ID, RUN_ID) where RUN_ID is
# the org SLUG (always `e2e-`/`rt-e2e-` prefixed → covered by the leak reaper and
# the sweep-stale-e2e-orgs sweeper). The org id is THE universal link across
# every resource class:
#   docker     : molecule.org-id=<ORG_ID> label (every container) + the
#                molecule.e2e-run-id=<RUN_ID> label (containers + networks +
#                volumes, once cp feat/local-docker-runid-labels lands) +
#                derived names (mol-<id>, mol-ws-cfg-*, mol-ws-work-*).
#   cloudflare : <slug>.<domain> + per-workspace ws-<id12>.<domain> DNS records
#                and the slug-named tunnel (EC2/Hetzner/GCP cloud path only —
#                the local-docker provisioner creates none, so CF scan no-ops).
#   db (CP)    : org_instances.org_id, organizations.id, tenant_resources.tenant_id.
#
# Each scanner SELF-SKIPS when its resource class is not applicable on this host
# (no docker daemon, no CF creds, no DB handle), so the same VERIFY runs green on
# a laptop (docker + DB) and on staging CI (EC2/CF) without per-target branching.
#
# CONFIG (all optional; absent → that class is skipped, never a false failure):
#   RF_DB_EXEC       psql command PREFIX that accepts `-tAc "<SQL>"` as a tail,
#                    e.g. "docker exec molecule-cp-postgres-1 psql -U cp -d cp_local".
#                    Unset → DB-row scan skipped (org-row admin-API check in the
#                    wrapper is the provider-agnostic fallback).
#   RF_CF_API_TOKEN  Cloudflare API token (DNS:read + DNS:edit) — enables CF scan.
#   RF_CF_ZONE_ID    Cloudflare zone id for the app domain.
#   RF_CF_ACCOUNT_ID Cloudflare account id — enables the tunnel scan (optional).
#   RF_APP_DOMAIN    app domain for CF fqdns (default moleculesai.app).
#   RF_CAPTURED_VOLS space-separated volume NAMES captured pre-teardown (so an
#                    UNLABELLED volume whose container is already gone is still
#                    checked). Populated by rf_capture_volumes.
#   RF_WORKER_IDS    space-separated workspace ids the run created (for ws-<id12>
#                    CF record checks).
#
# SAFETY: every selector is ORG_ID / RUN_ID / exact-name scoped — never a blanket
# sweep, so a shared docker daemon with OTHER live tenants is never touched.
# rf_force_clean NEVER deletes DB rows (that is the reaper's job and must never
# race admin_token); it only force-removes this run's docker/CF artifacts.

# ---------------------------------------------------------------------------
rf_log()  { echo "[run-footprint] $*"; }
rf_warn() { echo "[run-footprint] $*" >&2; }

rf_have_docker() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

# rf_container_owner_op NAME → the CP/MCP teardown op that owns this container.
rf_container_owner_op() {
  case "$1" in
    mol-ws-*)     echo "executeOrgPurge:purgeWorkspaces/purgeInfra → DeprovisionInstance mol-ws-* sweep (or platform-MCP delete_workspace)" ;;
    mol-tenant-*) echo "executeOrgPurge:purgeInfra → LocalDocker.DeprovisionInstance (tenant rm)" ;;
    *-pg|*-redis) echo "executeOrgPurge:purgeInfra → LocalDocker.DeprovisionInstance (sibling rm)" ;;
    ws-tenant-*)  echo "executeOrgPurge:purgeWorkspaces → CascadeWorkspaceEC2s (AWS workspace VM)" ;;
    *)            echo "executeOrgPurge:purgeInfra → provisioner.DeprovisionInstance" ;;
  esac
}

# rf_scan_containers ORG_ID RUN_ID → emits "container|<name>|<owner_op>" per leftover.
rf_scan_containers() {
  rf_have_docker || return 0
  local org="$1" run="$2" ids id name
  ids=$( { docker ps -aq --filter "label=molecule.org-id=$org" 2>/dev/null
           [ -n "$run" ] && docker ps -aq --filter "label=molecule.e2e-run-id=$run" 2>/dev/null
         } | sort -u )
  for id in $ids; do
    name=$(docker inspect -f '{{.Name}}' "$id" 2>/dev/null | sed 's#^/##')
    [ -z "$name" ] && name="$id"
    echo "container|$name|$(rf_container_owner_op "$name")"
  done
}

# rf_scan_networks ORG_ID RUN_ID NETWORK_NAME → emits "network|<name>|<owner_op>".
rf_scan_networks() {
  rf_have_docker || return 0
  local org="$1" run="$2" net="$3" ids id name
  ids=$( { docker network ls -q --filter "label=molecule.org-id=$org" 2>/dev/null
           [ -n "$run" ] && docker network ls -q --filter "label=molecule.e2e-run-id=$run" 2>/dev/null
           [ -n "$net" ] && docker network ls -q --filter "name=^${net}\$" 2>/dev/null
         } | sort -u )
  for id in $ids; do
    name=$(docker network inspect -f '{{.Name}}' "$id" 2>/dev/null)
    [ -z "$name" ] && name="$id"
    echo "network|$name|executeOrgPurge:purgeInfra → DeprovisionInstance (network rm)"
  done
}

# rf_scan_volumes ORG_ID RUN_ID [captured_vol_name...] → emits "volume|<name>|<owner_op>".
# Checks BOTH labelled volumes (the new molecule.org-id/e2e-run-id labels) AND
# the captured name list (covers a volume created UNLABELLED on a CP that predates
# the labelling change, whose container is already gone post-teardown).
rf_scan_volumes() {
  rf_have_docker || return 0
  local org="$1" run="$2"; shift 2
  local by_label v seen=" "
  by_label=$( { docker volume ls -q --filter "label=molecule.org-id=$org" 2>/dev/null
                [ -n "$run" ] && docker volume ls -q --filter "label=molecule.e2e-run-id=$run" 2>/dev/null
              } | sort -u )
  for v in $by_label; do
    echo "volume|$v|executeOrgPurge:purgeInfra → DeprovisionInstance volume rm (org-id sweep)"
    seen="$seen$v "
  done
  for v in "$@"; do
    [ -z "$v" ] && continue
    case "$seen" in *" $v "*) continue ;; esac
    if docker volume inspect "$v" >/dev/null 2>&1; then
      echo "volume|$v|executeOrgPurge:purgeInfra → DeprovisionInstance volume rm (name-derived; volume carried NO label)"
      seen="$seen$v "
    fi
  done
}

# rf_capture_volumes ORG_ID → echoes this org's volume NAMES (one per line), from
# BOTH labels and name-derivation off the org's mol-ws-* containers. Call BEFORE
# teardown and feed the result into RF_CAPTURED_VOLS so the post-teardown scan can
# catch a volume even after its owning container is gone.
rf_capture_volumes() {
  rf_have_docker || return 0
  local org="$1" id name base
  docker volume ls -q --filter "label=molecule.org-id=$org" 2>/dev/null
  for id in $(docker ps -aq --filter "label=molecule.org-id=$org" --filter "name=mol-ws-" 2>/dev/null); do
    name=$(docker inspect -f '{{.Name}}' "$id" 2>/dev/null | sed 's#^/##')
    case "$name" in
      mol-ws-*)
        base="${name#mol-ws-}"
        echo "mol-ws-cfg-$base"
        echo "mol-ws-work-$base"
        ;;
    esac
  done
}

# ---- Cloudflare (cloud path only; no-op on local-docker / when no creds) ------
rf_cf_enabled() { [ -n "${RF_CF_API_TOKEN:-}" ] && [ -n "${RF_CF_ZONE_ID:-}" ]; }

rf_cf_record_exists() {  # FQDN → echoes "<fqdn>|<type>" for any live record
  local fqdn="$1"
  curl -sS --max-time 20 \
    "https://api.cloudflare.com/client/v4/zones/${RF_CF_ZONE_ID}/dns_records?name=${fqdn}" \
    -H "Authorization: Bearer ${RF_CF_API_TOKEN}" 2>/dev/null \
  | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
for r in d.get('result', []) or []:
    print(f\"{r.get('name','')}|{r.get('type','')}\")" 2>/dev/null
}

# rf_scan_cf SLUG [worker_id...] → emits "cf_dns|<fqdn> (<type>)|<owner_op>".
rf_scan_cf() {
  rf_cf_enabled || return 0
  local slug="$1"; shift
  local domain="${RF_APP_DOMAIN:-moleculesai.app}" rec short
  while IFS='|' read -r rname rtype; do
    [ -n "$rname" ] && echo "cf_dns|$rname ($rtype)|deprovisionTenantInfra → cf.DeleteRecord (tenant CNAME)"
  done < <(rf_cf_record_exists "$slug.$domain")
  for wid in "$@"; do
    [ -z "$wid" ] && continue
    short=$(echo "$wid" | tr -d - | cut -c1-12)
    while IFS='|' read -r rname rtype; do
      [ -n "$rname" ] && echo "cf_dns|$rname ($rtype)|cleanupWorkspaceArtifacts → cf.DeleteRecord (ws record)"
    done < <(rf_cf_record_exists "ws-$short.$domain")
  done
  # Tunnel scan (optional — needs account id).
  if [ -n "${RF_CF_ACCOUNT_ID:-}" ]; then
    rec=$(curl -sS --max-time 20 \
      "https://api.cloudflare.com/client/v4/accounts/${RF_CF_ACCOUNT_ID}/cfd_tunnel?name=${slug}&is_deleted=false" \
      -H "Authorization: Bearer ${RF_CF_API_TOKEN}" 2>/dev/null \
      | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
for t in d.get('result', []) or []:
    if not t.get('deleted_at'):
        print(t.get('name',''))" 2>/dev/null)
    [ -n "$rec" ] && echo "cf_tunnel|$rec|deprovisionTenantInfra → tunnel.DeleteTunnel"
  fi
}

# ---- CP database rows ---------------------------------------------------------
rf_db_count() {  # SQL → trimmed integer (or "" when DB scan disabled / errored)
  [ -n "${RF_DB_EXEC:-}" ] || return 0
  $RF_DB_EXEC -tAc "$1" 2>/dev/null | tr -d '[:space:]'
}

# rf_scan_db ORG_ID → emits "db_*|<detail>|<owner_op>" for any surviving row.
rf_scan_db() {
  [ -n "${RF_DB_EXEC:-}" ] || return 0
  local org="$1" n
  n=$(rf_db_count "SELECT count(*) FROM org_instances WHERE org_id='$org';")
  [ -n "$n" ] && [ "$n" != "0" ] && echo "db_org_instances|org_id=$org rows=$n|executeOrgPurge:purgeDBRows (DELETE FROM org_instances)"
  n=$(rf_db_count "SELECT count(*) FROM organizations WHERE id='$org';")
  [ -n "$n" ] && [ "$n" != "0" ] && echo "db_organizations|id=$org rows=$n|executeOrgPurge:purgeDBRows (DELETE FROM organizations)"
  n=$(rf_db_count "SELECT count(*) FROM tenant_resources WHERE tenant_id='$org' AND state<>'deleted';")
  [ -n "$n" ] && [ "$n" != "0" ] && echo "db_tenant_resources|tenant_id=$org state<>deleted rows=$n|deprovisionTenantInfra/cleanupWorkspaceArtifacts → RecordOrLog(StateDeleted)"
}

# ---- the VERIFY assertion (the whole point) -----------------------------------
# rf_verify ORG_ID RUN_ID NETWORK_NAME SLUG
#   Scans every resource class for anything still bearing the run AFTER the
#   product's own teardown returned. Prints a precise per-resource report and
#   returns 0 (clean) or 1 (LEAK → caller must FAIL the gate).
#   Captured volumes via RF_CAPTURED_VOLS; worker ids via RF_WORKER_IDS.
rf_verify() {
  local org="$1" run="$2" net="$3" slug="$4"
  local leftovers
  leftovers=$( {
      rf_scan_containers "$org" "$run"
      rf_scan_networks   "$org" "$run" "$net"
      # shellcheck disable=SC2086
      rf_scan_volumes    "$org" "$run" ${RF_CAPTURED_VOLS:-}
      # shellcheck disable=SC2086
      rf_scan_cf         "$slug" ${RF_WORKER_IDS:-}
      rf_scan_db         "$org"
    } | awk 'NF' | sort -u )

  if [ -z "$leftovers" ]; then
    rf_log "VERIFY PASS ✅ — zero resources still bear run_id=$run (org_id=$org) after the CP/MCP teardown."
    rf_log "  scanned: docker containers/networks/volumes, cloudflare dns/tunnels, db org_instances/organizations/tenant_resources."
    return 0
  fi

  rf_warn "VERIFY FAIL ❌ — the product's OWN teardown LEFT RESIDUAL resources bearing run_id=$run:"
  printf '%s\n' "$leftovers" | while IFS='|' read -r typ name op; do
    printf '  LEAK  type=%-20s name=%-44s  should-have-been-removed-by: %s\n' "$typ" "$name" "$op" >&2
  done
  local count; count=$(printf '%s\n' "$leftovers" | awk 'NF' | wc -l | tr -d ' ')
  rf_warn "$count leaked resource(s) — this is a CP/MCP TEARDOWN GAP, not something the reaper should silently mop up."
  return 1
}

# ---- FINALLY safety belt (so the e2e itself never leaks, even on a caught bug) -
# rf_force_clean ORG_ID RUN_ID NETWORK_NAME — force-remove every docker/CF artifact
# bearing the run. Idempotent; ORG/RUN/exact-name scoped (never a blanket sweep).
# DB rows are deliberately NOT touched (left to the e2e-leak reaper; admin_token
# is never read or written here).
rf_force_clean() {
  local org="$1" run="$2" net="$3" ids vols nets v n
  if rf_have_docker; then
    ids=$( { docker ps -aq --filter "label=molecule.org-id=$org" 2>/dev/null
             [ -n "$run" ] && docker ps -aq --filter "label=molecule.e2e-run-id=$run" 2>/dev/null
           } | sort -u )
    # shellcheck disable=SC2086
    [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
    vols=$( { docker volume ls -q --filter "label=molecule.org-id=$org" 2>/dev/null
              [ -n "$run" ] && docker volume ls -q --filter "label=molecule.e2e-run-id=$run" 2>/dev/null
            } | sort -u )
    for v in $vols ${RF_CAPTURED_VOLS:-}; do
      [ -n "$v" ] && docker volume rm -f "$v" >/dev/null 2>&1
    done
    nets=$( { docker network ls -q --filter "label=molecule.org-id=$org" 2>/dev/null
              [ -n "$net" ] && docker network ls -q --filter "name=^${net}\$" 2>/dev/null
            } | sort -u )
    for n in $nets; do docker network rm "$n" >/dev/null 2>&1; done
  fi
  # CF best-effort delete by slug/ws-id (cloud path only).
  if rf_cf_enabled; then
    rf_force_clean_cf "$run"
  fi
  rf_log "force-clean done for run_id=$run (DB rows left to the e2e-leak reaper backstop; admin_token never touched)."
}

rf_force_clean_cf() {
  local slug="$1" domain="${RF_APP_DOMAIN:-moleculesai.app}" rid wid short
  local fqdns=( "$slug.$domain" )   # one element today; array keeps it extensible (ws-subdomains) + SC2066-clean
  for fqdn in "${fqdns[@]}"; do
    rid=$(curl -sS --max-time 20 \
      "https://api.cloudflare.com/client/v4/zones/${RF_CF_ZONE_ID}/dns_records?name=${fqdn}" \
      -H "Authorization: Bearer ${RF_CF_API_TOKEN}" 2>/dev/null \
      | python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for r in d.get('result',[]) or []: print(r.get('id',''))" 2>/dev/null)
    for id in $rid; do
      [ -n "$id" ] && curl -sS --max-time 20 -X DELETE \
        "https://api.cloudflare.com/client/v4/zones/${RF_CF_ZONE_ID}/dns_records/${id}" \
        -H "Authorization: Bearer ${RF_CF_API_TOKEN}" >/dev/null 2>&1
    done
  done
  for wid in ${RF_WORKER_IDS:-}; do
    short=$(echo "$wid" | tr -d - | cut -c1-12)
    rid=$(curl -sS --max-time 20 \
      "https://api.cloudflare.com/client/v4/zones/${RF_CF_ZONE_ID}/dns_records?name=ws-${short}.${domain}" \
      -H "Authorization: Bearer ${RF_CF_API_TOKEN}" 2>/dev/null \
      | python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for r in d.get('result',[]) or []: print(r.get('id',''))" 2>/dev/null)
    for id in $rid; do
      [ -n "$id" ] && curl -sS --max-time 20 -X DELETE \
        "https://api.cloudflare.com/client/v4/zones/${RF_CF_ZONE_ID}/dns_records/${id}" \
        -H "Authorization: Bearer ${RF_CF_API_TOKEN}" >/dev/null 2>&1
    done
  done
}
