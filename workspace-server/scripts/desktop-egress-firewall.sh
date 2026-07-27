#!/usr/bin/env bash
# desktop-egress-firewall.sh — the decision-1 / §6.1 egress-deny for desktop
# sidecars, delivered as an operator-run host firewall.
#
# WHY THIS EXISTS (verified 2026-07-27, Docker 29.5.3):
#   A desktop sidecar on its dedicated per-workspace network is ALREADY isolated
#   from other Docker networks by-IP (Docker's DOCKER-ISOLATION chains drop
#   cross-bridge traffic). The residual exposure a per-workspace network does NOT
#   close is:
#     • the host gateway — so any backend infra that PUBLISHES a port to the host
#       (Postgres/Redis/MinIO/LiteLLM via `-p`) is reachable from the sidecar via
#       the gateway IP;  ← the real vulnerability
#     • the cloud metadata IP 169.254.169.254 (credential theft on EC2/GCE/etc).
#   This script installs DOCKER-USER rules that DENY those destinations from the
#   desktop networks' subnets while leaving public-internet browsing intact
#   (internet-bound packets have public destinations and are untouched).
#
#   Empirically verified: with these rules a sidecar can no longer reach a
#   host-published infra port or the metadata IP, but 1.1.1.1:443 still succeeds.
#
# WHY A SCRIPT, NOT APP CODE:
#   Mutating the host's DOCKER-USER chain requires host NET_ADMIN. The
#   workspace-server runs with only the Docker socket, by design — having it
#   launch privileged/host-network helper containers to edit host iptables would
#   expand its blast radius far more than the vulnerability this closes. Host
#   firewall config is operator territory (like userns-remap). Run this on each
#   Docker host that runs desktop sidecars — on a timer (systemd/cron, e.g. every
#   60s) or from your host-provisioning — so newly-created per-workspace networks
#   are covered as they appear. It is idempotent.
#
# For the CP/k8s backend, use the declarative equivalent instead:
#   workspace-server/deploy/desktop-egress-networkpolicy.yaml
#
# USAGE:
#   sudo ./desktop-egress-firewall.sh install   # add deny rules for all desktop nets
#   sudo ./desktop-egress-firewall.sh remove    # remove them
#   sudo ./desktop-egress-firewall.sh status    # show current rules
#
# Requires: docker (to enumerate desktop-labeled networks), iptables, root.

set -euo pipefail

ROLE_LABEL="molecule.platform.role=desktop"
# Destinations denied FROM each desktop subnet. Covers every Docker default pool
# (172.16/12, 192.168/16) plus 10/8, plus the link-local cloud metadata IP.
DENY_DESTS=("169.254.169.254/32" "10.0.0.0/8" "172.16.0.0/12" "192.168.0.0/16")

need_root() { [ "$(id -u)" -eq 0 ] || { echo "must run as root (iptables)" >&2; exit 1; }; }

# Subnets of all networks labelled as desktop networks.
desktop_subnets() {
  local nets
  nets=$(docker network ls --filter "label=${ROLE_LABEL}" -q)
  [ -n "$nets" ] || return 0
  # shellcheck disable=SC2086
  docker network inspect $nets \
    --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' | grep -v '^$' | sort -u
}

rule_exists() { iptables -C DOCKER-USER -s "$1" -d "$2" -j DROP 2>/dev/null; }

install_rules() {
  need_root
  local subnet dest added=0
  while IFS= read -r subnet; do
    [ -n "$subnet" ] || continue
    for dest in "${DENY_DESTS[@]}"; do
      if ! rule_exists "$subnet" "$dest"; then
        iptables -I DOCKER-USER -s "$subnet" -d "$dest" -j DROP
        added=$((added+1))
      fi
    done
  done < <(desktop_subnets)
  echo "desktop-egress-firewall: install complete (${added} rule(s) added)"
}

remove_rules() {
  need_root
  local subnet dest removed=0
  while IFS= read -r subnet; do
    [ -n "$subnet" ] || continue
    for dest in "${DENY_DESTS[@]}"; do
      while rule_exists "$subnet" "$dest"; do
        iptables -D DOCKER-USER -s "$subnet" -d "$dest" -j DROP
        removed=$((removed+1))
      done
    done
  done < <(desktop_subnets)
  echo "desktop-egress-firewall: remove complete (${removed} rule(s) removed)"
}

status_rules() {
  need_root
  echo "Desktop networks (label ${ROLE_LABEL}):"
  desktop_subnets | sed 's/^/  /' || true
  echo "DOCKER-USER chain:"
  iptables -L DOCKER-USER -n --line-numbers
}

case "${1:-}" in
  install) install_rules ;;
  remove)  remove_rules ;;
  status)  status_rules ;;
  *) echo "usage: $0 {install|remove|status}" >&2; exit 2 ;;
esac
