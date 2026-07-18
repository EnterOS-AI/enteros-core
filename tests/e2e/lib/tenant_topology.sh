#!/usr/bin/env bash
# tenant_topology.sh — derive the tenant-facing routing/CORS topology for a
# staging E2E scenario, so the SAME script runs unchanged against either
#   (a) real staging  — each tenant front-doored at its own slug.<domain>, OR
#   (b) an ephemeral CP — one throwaway container whose wildcard proxy resolves
#       the tenant by SLUG (Host + X-Molecule-Org-Slug), reached via the CP base.
#
# This is the proven topology contract extracted verbatim from
# test_staging_concierge_e2e.sh (the #4406 ephemeral port) so every remaining
# #86 scenario port reuses ONE implementation instead of re-deriving it (the
# three copies the code review flagged). Default (all MOLECULE_TENANT_* unset)
# reproduces exact staging behaviour byte-for-byte.
#
# derive_tenant_topology <slug> <cp_url>
#   Sets in the CALLER's scope (do not declare these local in the caller):
#     TENANT_URL         base URL the scenario hits for tenant-proxied calls
#     TENANT_ROUTE_HOST  the Host header value under ephemeral slug-routing ('' on staging)
#     TENANT_ROUTE_HDRS  bash array: (-H Host: … -H X-Molecule-Org-Slug: …) or empty on staging
#     TENANT_ORIGIN      the exact Origin the tenant's gin-contrib/cors allows
#   Returns 0 on success; 1 if an ephemeral CORS Origin cannot be derived
#   (route-routing active but neither an origin template nor a usable scheme).
#
# Env inputs (all optional; unset ⇒ staging):
#   MOLECULE_TENANT_URL             base URL override (ephemeral ⇒ the CP base URL)
#   MOLECULE_TENANT_DOMAIN          override the derived staging tenant domain
#   MOLECULE_TENANT_ROUTE_HOST      explicit Host (else <slug>.<ROUTE_DOMAIN>)
#   MOLECULE_TENANT_ROUTE_DOMAIN    ephemeral route domain (e.g. lvh.me)
#   MOLECULE_TENANT_ROUTE_PORT      explicit origin port (else taken from TENANT_URL)
#   MOLECULE_TENANT_ORIGIN_TEMPLATE the CP's LOCAL_TENANT_URL_TEMPLATE with {slug}
derive_tenant_topology() {
  local slug="$1" cp_url="$2"
  local cp_host derived_domain tenant_domain

  cp_host="${cp_url#*://}"
  cp_host="${cp_host%%/*}"
  case "$cp_host" in
    api.*)         derived_domain="${cp_host#api.}" ;;
    staging-api.*) derived_domain="staging.${cp_host#staging-api.}" ;;
    *)             derived_domain="$cp_host" ;;
  esac
  tenant_domain="${MOLECULE_TENANT_DOMAIN:-$derived_domain}"

  # Base URL: ephemeral runner points this at the CP base URL; default (unset)
  # keeps the exact staging slug.<domain> subdomain.
  TENANT_URL="${MOLECULE_TENANT_URL:-https://$slug.$tenant_domain}"

  # Ephemeral slug-routing headers: carry the routing slug via Host +
  # X-Molecule-Org-Slug so the CP wildcard proxy resolves the tenant. Default
  # unset ⇒ no extra headers ⇒ exact staging behaviour.
  TENANT_ROUTE_HOST="${MOLECULE_TENANT_ROUTE_HOST:-}"
  if [ -z "$TENANT_ROUTE_HOST" ] && [ -n "${MOLECULE_TENANT_ROUTE_DOMAIN:-}" ]; then
    TENANT_ROUTE_HOST="$slug.$MOLECULE_TENANT_ROUTE_DOMAIN"
  fi
  TENANT_ROUTE_HDRS=()
  if [ -n "$TENANT_ROUTE_HOST" ]; then
    TENANT_ROUTE_HDRS=(-H "Host: $TENANT_ROUTE_HOST" -H "X-Molecule-Org-Slug: $slug")
  fi

  # Origin: the tenant's gin-contrib/cors allows exactly ONE origin — its own
  # public front-door (CORS_ORIGINS). Precedence (see the #4406 port):
  #   1. MOLECULE_TENANT_ORIGIN_TEMPLATE → the SAME template the CP turns into the
  #      tenant's CORS_ORIGINS, substituted with this slug (byte-identical). Wins.
  #   2. ephemeral slug-routing active but template unset → $TENANT_URL is the CP
  #      base URL (NOT a tenant origin), so derive a tenant-scoped origin from the
  #      route host + the scheme/port of $TENANT_URL.
  #   3. staging (no routing) → Origin=$TENANT_URL (the tenant's own subdomain,
  #      which IS its CORS_ORIGINS) ⇒ exact staging behaviour.
  TENANT_ORIGIN="$TENANT_URL"
  if [ -n "${MOLECULE_TENANT_ORIGIN_TEMPLATE:-}" ]; then
    TENANT_ORIGIN="${MOLECULE_TENANT_ORIGIN_TEMPLATE//\{slug\}/$slug}"
  elif [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    local origin_scheme origin_port tu_hostport
    origin_scheme="${TENANT_URL%%://*}"
    case "$origin_scheme" in http|https) ;; *) origin_scheme="" ;; esac
    origin_port="${MOLECULE_TENANT_ROUTE_PORT:-}"
    if [ -z "$origin_port" ]; then
      tu_hostport="${TENANT_URL#*://}"
      tu_hostport="${tu_hostport%%/*}"
      case "$tu_hostport" in *:*) origin_port="${tu_hostport##*:}" ;; esac
    fi
    if [ -z "$origin_scheme" ] || [ -z "$TENANT_ROUTE_HOST" ]; then
      echo "[tenant-topology] Cannot derive a tenant CORS Origin for ephemeral slug-routing (scheme='$origin_scheme' route_host='$TENANT_ROUTE_HOST'). Set MOLECULE_TENANT_ORIGIN_TEMPLATE to the CP's LOCAL_TENANT_URL_TEMPLATE (with {slug})." >&2
      return 1
    fi
    case "$TENANT_ROUTE_HOST" in
      *:*) TENANT_ORIGIN="${origin_scheme}://${TENANT_ROUTE_HOST}" ;;
      *)   TENANT_ORIGIN="${origin_scheme}://${TENANT_ROUTE_HOST}${origin_port:+:$origin_port}" ;;
    esac
  fi
  return 0
}
