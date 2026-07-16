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
# WHAT IS (AND IS NOT) RETRYABLE — the non-masking contract
#   RETRY only the two signatures that mean "the request never reached a
#   request handler", so a retry cannot mask a real create regression and
#   cannot duplicate a non-idempotent POST:
#     • an EMPTY-body 503  — Service Unavailable synthesised by the edge/ingress
#       before a handler is up (the exact RCA'd cold-origin signature); and
#     • an EMPTY status (curl `000` / connection refused|reset) — the same cold
#       window observed at the transport layer, mirroring lib/create_org_with_retry.sh.
#   DO NOT retry:
#     • a NON-EMPTY body — a real app error (422/400/… JSON) surfaces on the
#       FIRST try; the Go handler always emits a body, so emptiness IS the
#       "not the handler" signal that keeps a genuine regression from masking.
#     • 502 / 504 — bad-gateway / gateway-timeout can mean the origin already
#       PROCESSED the create before the response was lost; re-POSTing a
#       non-idempotent `POST /workspaces` could duplicate the workspace. These
#       fail RED on the first try (a persistent one is a real problem anyway).
#   A persistent 503/000 exhausts the caller's bounded attempt budget and still
#   fails RED with the full header diagnostic; every attempt is logged, so a
#   transient that self-heals is visible, never a silent green.
#
#   create_should_retry_cold  <http_status> <body>   → rc 0 retry / rc 1 don't
#   create_parse_status       <headers_file>  → status code from the FINAL status line
#   create_parse_retry_after  <headers_file>  → Retry-After delta-seconds
#                                                (integer only; default 2, cap 10)
#   create_parse_server       <headers_file>  → `server:` header value (who answered)

# Retry only the cold-origin "never reached a handler" signatures (see contract).
create_should_retry_cold() {
  local status="$1" body="$2"
  # A non-empty body is a real response from the app (e.g. a 422/400 JSON
  # error). Never retry it — surface it so the caller's id-check names WHY.
  if printf '%s' "$body" | grep -q '[^[:space:]]'; then
    return 1
  fi
  case "$status" in
    503) return 0 ;;      # edge/ingress "Service Unavailable" — handler not up yet
    "" | 000) return 0 ;; # connection refused/reset in the cold window (no status line)
    *) return 1 ;;        # 502/504 (maybe-processed → non-idempotent) and all else
  esac
}

# The FINAL status line of a curl -D dump is the real response status. curl
# prepends interim 1xx lines (`HTTP/1.1 100 Continue` for an Expect:
# 100-continue on a large create payload; a `103 Early Hints` block), so
# `head -1` would parse the interim code — take the LAST `HTTP/` line instead.
create_parse_status() {
  grep -iE '^HTTP/' "$1" 2>/dev/null | tail -1 | awk '{print $2}' | tr -dc '0-9'
}

# RFC 7231 allows Retry-After to be delta-seconds OR an HTTP-date. We only honor
# a bare integer delta; anything else (an HTTP-date, empty, or junk) falls back
# to the default rather than being mangled by digit-stripping into a huge value.
create_parse_retry_after() {
  local raw
  raw=$(grep -i '^retry-after:' "$1" 2>/dev/null | head -1 \
    | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r\n' | tr -d '[:space:]')
  if printf '%s' "$raw" | grep -qE '^[0-9]+$'; then
    # A cold-start window is seconds; cap a hostile/large value so it can't
    # stall the whole gate.
    [ "$raw" -gt 10 ] && raw=10
    printf '%s' "$raw"
  else
    printf '2'
  fi
}

create_parse_server() {
  grep -i '^server:' "$1" 2>/dev/null | head -1 \
    | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r\n'
}
