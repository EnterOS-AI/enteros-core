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
#   create_should_retry_cold  <http_status> <body> [curl_exit]  → rc 0 retry / rc 1 don't
#   create_parse_status       <headers_file>  → status code from the FINAL status line
#   create_parse_retry_after  <headers_file>  → Retry-After delta-seconds
#                                                (integer only; default 2, cap 10)
#   create_parse_server       <headers_file>  → `server:` header value (who answered)

# A curl transport failure (empty status line, empty body) is NOT uniformly
# retryable: a CLIENT-side timeout may fire AFTER the origin already processed
# the non-idempotent POST, so re-POSTing double-creates — the exact 502/504
# "maybe-processed" hazard, at the transport layer. Only a connection that was
# refused/reset/never-established (curl couldn't-resolve/connect/SSL/recv) truly
# "never reached a handler" and is safe to retry. These are the curl exit codes
# that mean "never established / reset before the server could act":
#   6  couldn't resolve host      7  failed to connect (refused)
#   35 SSL connect error          56 failure receiving data (connection reset)
# curl 28 (operation timeout) and every OTHER/unknown transport exit are treated
# as maybe-processed → NOT retried (non-masking default: when unsure, don't
# double-create). Mirrors the Go coldCreateTransportRetryable + the TS
# classifier refusing TimeoutError/AbortError.
_CREATE_RETRYABLE_CURL_EXITS=" 6 7 35 56 "

# Retry only the cold-origin "never reached a handler" signatures (see contract).
# curl_exit is OPTIONAL: pass curl's exit status on a transport failure so a
# client timeout (28) is distinguished from a connection reset/refusal. When
# omitted or 0, the decision is purely status/body based (a response WAS
# received), preserving the original two-arg behavior.
create_should_retry_cold() {
  local status="$1" body="$2" curl_exit="${3:-}"
  # A non-empty body is a real response from the app (e.g. a 422/400 JSON
  # error). Never retry it — surface it so the caller's id-check names WHY.
  if printf '%s' "$body" | grep -q '[^[:space:]]'; then
    return 1
  fi
  # Transport-layer failure: ONLY when curl produced no status line at all (an
  # empty status). A non-empty status means a response WAS received — even a
  # 503 makes curl --fail-with-body exit 22 — so it must be classified on the
  # status below, never mistaken for a transport failure. With no status line,
  # curl's exit decides: a connection reset/refused/never-established is the
  # retryable cold twin; a client timeout (28) or any other/unknown transport
  # exit is maybe-processed → do NOT retry.
  if [ -z "$status" ] && [ -n "$curl_exit" ] && [ "$curl_exit" != "0" ]; then
    case "$_CREATE_RETRYABLE_CURL_EXITS" in
      *" $curl_exit "*) return 0 ;;  # refused/reset/never-established → cold twin
      *) return 1 ;;                 # 28 timeout & any other transport exit → no retry
    esac
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
    # stall the whole gate. Guard the cap against integer overflow: a 20+digit
    # all-numeric value passes the ^[0-9]+$ test but overflows the shell's
    # signed-64-bit `[ "$raw" -gt 10 ]` arithmetic ("integer expression
    # expected"), which — untrapped — skips the cap and sleeps a giant value
    # until CI times out. Any value with more digits than the cap is, by
    # construction, larger than the cap → clamp by LENGTH before the numeric
    # compare ever runs. (Strip a leading-zero run first so "0000000010" is
    # length-10, not spuriously clamped.)
    local n="${raw#"${raw%%[!0]*}"}"   # drop leading zeros
    [ -z "$n" ] && n=0                  # all-zeros → 0
    if [ "${#n}" -gt 2 ]; then
      raw=10                            # >=100: always over the 10s cap
    elif [ "$n" -gt 10 ]; then
      raw=10                            # 2 digits (<=99): numeric compare is overflow-safe
    else
      raw="$n"                          # already <= cap
    fi
    printf '%s' "$raw"
  else
    printf '2'
  fi
}

create_parse_server() {
  grep -i '^server:' "$1" 2>/dev/null | head -1 \
    | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r\n'
}
