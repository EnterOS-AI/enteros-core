#!/usr/bin/env bash
# workspace_create_retry.sh — pure, unit-testable classifier + header parsers
# for the staging full-SaaS E2E's parent/child `POST /workspaces` retry loop.
#
# WHY THIS EXISTS
# The create POST intermittently returns a 503 with a COMPLETELY EMPTY body,
# ~1s after step 4 declared the tenant reachable (health-green). core#4307
# added header capture; the captured headers name the responder as Cloudflare
# (`server: cloudflare`, `cf-ray: …`, `content-length: 0`, `retry-after: 2`) —
# i.e. the tenant origin is briefly not accepting writes in the cold window
# between "health answers" and "create path is up". A real client retries such
# a transient with the origin's own Retry-After; the E2E previously failed on
# the first 503. This lib is the seam the retry loop classifies against, kept
# pure (no curl, no globals) so it can be exercised offline with fixtures.
#
# CONTRACT
#   create_is_transient_cold_5xx <http_status> <body>
#     rc 0  → transient cold-origin 5xx worth retrying (empty body + 502/503/504)
#     rc 1  → NOT retryable: a non-empty body is a real app error (422/400/…),
#             and any non-5xx status is surfaced immediately.
#     Fail-CLOSED on ambiguity: only an EMPTY-body 502/503/504 retries. This is
#     what keeps the retry from masking a genuine create regression — a real
#     error emits a JSON body, which is never retried, and a persistent outage
#     exhausts the caller's attempt budget and still goes RED.
#
#   create_parse_status       <headers_file>  → status code from the status line
#   create_parse_retry_after  <headers_file>  → Retry-After seconds (default 2,
#                                                capped at 10 vs a hostile value)
#   create_parse_server       <headers_file>  → `server:` header value (who answered)

# Retry only the empty-body cold-origin 5xx. Everything else is caller-visible.
create_is_transient_cold_5xx() {
  local status="$1" body="$2"
  # A non-empty body is a real response from the app (e.g. a 422/400 JSON
  # error). Never retry it — surface it so the caller's id-check names WHY.
  if printf '%s' "$body" | grep -q '[^[:space:]]'; then
    return 1
  fi
  case "$status" in
    502 | 503 | 504) return 0 ;;
    *) return 1 ;;
  esac
}

# First status line of a curl -D dump is "HTTP/2 503" or
# "HTTP/1.1 503 Service Unavailable"; field 2 is the code either way.
create_parse_status() {
  head -1 "$1" 2>/dev/null | awk '{print $2}' | tr -dc '0-9'
}

create_parse_retry_after() {
  local ra
  ra=$(grep -i '^retry-after:' "$1" 2>/dev/null | head -1 | tr -dc '0-9')
  [ -n "$ra" ] || ra=2
  # A cold-start window is seconds; cap a hostile/edge Retry-After so a bad
  # value can't stall the whole gate.
  [ "$ra" -gt 10 ] && ra=10
  printf '%s' "$ra"
}

create_parse_server() {
  grep -i '^server:' "$1" 2>/dev/null | head -1 \
    | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r' | tr -d '\n'
}
