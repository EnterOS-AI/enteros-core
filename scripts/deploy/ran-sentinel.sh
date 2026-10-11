#!/usr/bin/env bash
# ran-sentinel — "did that job's body actually execute?", as a shell library.
#
# WHY THIS FILE EXISTS
# --------------------
# Gitea's `PickTask` COMMITS a task row as `running` with `runner_id` set, and
# only THEN assembles the FetchTask payload and returns it. FetchTask is
# at-most-once: there is no ack, and nothing re-queues a claimed-but-undelivered
# task. With act_runner's `fetch_timeout: 5s`, a lost response means the runner
# silently returns having logged nothing, while the claim is already committed.
# `StopZombieTasks` then reaps the row as **failure** 10-15 minutes later.
#
# The reaped row is INDISTINGUISHABLE, through `needs.<job>.result`, from a job
# that ran and genuinely failed. Its verified signature in the task table is
#   status=failure AND log_length=0 AND NOT log_in_storage AND NOT log_expired
#   AND updated<=started AND duration in [600,900]
# and of 288,338 tasks in retention 314 match, all 314 being the pathology; no
# successful task has log_length=0 with started>0. The rate is rising: 0.006%
# mid-July, 0.28% last week, 0.964% today. Every instance is on a persistent
# fleet runner.
#
# CONSEQUENCE THIS LIBRARY EXISTS TO STOP. staging-tenant-cd.yml advances the
# staging tenant-image pin BEFORE testing it, and `rollback-pin` decides in-shell
# from `E2E_RESULT`. Two staging pin rollbacks were therefore triggered purely by
# a phantom red — `candidate path failed after pin advance (redeploy=success
# readiness=success e2e=failure)`, with CP audit 40708 landing 23 seconds after
# the reaper stamped task 989633 failed. It cuts both ways: `rollback-pin` task
# 885373 was itself a phantom, so a genuinely-failed candidate stayed pinned
# because its rollback was never delivered.
#
# THE MECHANISM
# -------------
# Every covered job emits a marker as its FIRST step (`begin`) and again as its
# LAST step (`end`, under `if: always()`). Both markers are carried out of the
# job as job `outputs:`, so a consumer reads them from `needs.<job>.outputs.*`.
#
#   * `begin` present  => the job body was dispatched and started executing.
#                         A task that was claimed but never delivered cannot
#                         emit it: NO step of that job ever ran.
#   * `end` present    => the job body additionally reached its final step, so
#                         it was not killed part-way through.
#
# The two together distinguish three states that `needs.<job>.result` collapses
# into one: never-ran, ran-and-was-killed, and ran-to-completion.
#
# NON-VACUITY. The token is not a boolean and not a fixed string: it embeds
# `GITHUB_RUN_ID`, and the consumer RECOMPUTES the expected token from its own
# environment and compares for exact equality. `ran_sentinel_expect` REFUSES to
# produce a token when `GITHUB_RUN_ID` is empty or unset — because an empty
# expectation would compare equal to an empty (absent) sentinel and every check
# in this file would silently become a no-op. That refusal is the single most
# important line here; it is pinned by a test.
#
# ASYMMETRY (the safety property). A missing sentinel may only ever SUPPRESS a
# rollback; it must never CAUSE one. Rolling the fleet is the destructive
# direction, so it requires POSITIVE evidence that the job which condemned the
# candidate actually ran. Absence of that evidence is an infra fault to report
# loudly, never a condition to act on and never a condition to retry.
#
# THIS IS NOT A RETRY. Nothing here reruns, degrades, or continues-on-error. A
# missing sentinel makes the run RED and leaves the fleet alone.
#
# SSOT NOTE. The `begin`/`end` token TEXT is also written inline in the emitting
# steps of .gitea/workflows/staging-tenant-cd.yml, because those steps must run
# BEFORE `actions/checkout` (a marker that needs the repo on disk is not a
# marker that the job started). The two are cross-checked by
# .gitea/scripts/tests/test_ran_sentinel.py, which executes the real YAML step
# bodies and compares what they emit against `ran_sentinel_expect` here — so
# drift between the emitter and the verifier is a red test, not a silent hole.
#
# shellcheck shell=bash

# ran_sentinel_expect <job-key> <begin|end>
#
# Prints the exact token the named job must emit for the named phase of THIS
# run. Fails (rc 2) rather than printing an empty or run-independent token.
ran_sentinel_expect() {
  local job="${1-}" phase="${2-}"
  if [ -z "$job" ]; then
    echo "::error::ran_sentinel_expect: job key is required" >&2
    return 2
  fi
  case "$phase" in
    begin | end) ;;
    *)
      echo "::error::ran_sentinel_expect: phase must be 'begin' or 'end', got '${phase}'" >&2
      return 2
      ;;
  esac
  local run_id="${GITHUB_RUN_ID-}"
  if [ -z "$run_id" ]; then
    # THE non-vacuity guard. With an empty run id the expected token would be
    # run-independent, and — worse — a caller that built it by naive string
    # concatenation could produce a token that an ABSENT sentinel matches. Fail
    # closed instead: no comparison at all is safer than a comparison that
    # cannot fail.
    echo "::error::ran-sentinel: GITHUB_RUN_ID is empty or unset, so the expected token would not be bound to this run. Refusing to build an unfalsifiable expectation." >&2
    return 2
  fi
  printf 'ran-sentinel/v1 %s %s run=%s' "$job" "$phase" "$run_id"
}

# ran_sentinel_classify <job-key> <result> <begin-token> <end-token>
#
# Prints exactly one verdict on stdout:
#
#   not-dispatched      the job was `skipped`, so Gitea never created a task for
#                       it. Absence of a sentinel is EXPECTED and is not a fault;
#                       an upstream gate already decided this run.
#   phantom             the job has a terminal result but never emitted `begin`.
#                       Its result is NOT a verdict — the body never ran.
#   phantom-green       the job reports `success` but never emitted `end`. A
#                       green that did not reach its own last step is not a
#                       green we may act on.
#   ran-failed-partial  the job started and did not reach its last step, and its
#                       result is not success. It really ran; the verdict stands.
#   ran-failed          the job ran start to finish and its result is not
#                       success. A genuine, trustworthy failure.
#   ran-ok              the job ran start to finish and succeeded.
#
# rc 2 on malformed input (empty result, bad job key, unusable GITHUB_RUN_ID).
ran_sentinel_classify() {
  local job="${1-}" result="${2-}" begin="${3-}" end="${4-}"
  local exp_begin exp_end

  if [ -z "$result" ]; then
    echo "::error::ran_sentinel_classify: empty result for job '${job}'. An absent needs.<job>.result is not a verdict; refusing to classify." >&2
    return 2
  fi

  # Both expectations are computed BEFORE any comparison, and either one failing
  # aborts. This is what makes the comparisons below non-vacuous: `exp_begin`
  # and `exp_end` are guaranteed non-empty by construction.
  exp_begin="$(ran_sentinel_expect "$job" begin)" || return 2
  exp_end="$(ran_sentinel_expect "$job" end)" || return 2

  if [ "$result" = "skipped" ]; then
    printf 'not-dispatched'
    return 0
  fi

  if [ "$begin" != "$exp_begin" ]; then
    printf 'phantom'
    return 0
  fi

  if [ "$result" = "success" ]; then
    if [ "$end" != "$exp_end" ]; then
      printf 'phantom-green'
      return 0
    fi
    printf 'ran-ok'
    return 0
  fi

  if [ "$end" != "$exp_end" ]; then
    printf 'ran-failed-partial'
    return 0
  fi
  printf 'ran-failed'
}

# ran_sentinel_decide <job> <result> <begin> <end> [<job> <result> <begin> <end> ...]
#
# Aggregates the per-job verdicts of every job whose result feeds the rollback
# decision, and prints exactly one decision word on stdout:
#
#   INFRA_FAULT   at least one job's result is not a verdict (phantom /
#                 phantom-green). FAIL LOUD AND DO NOT TOUCH THE FLEET.
#   ROLLBACK      no phantoms, and at least one job verifiably failed or was
#                 legitimately skipped. Revert exactly as before.
#   NO_ROLLBACK   every job ran start to finish and succeeded.
#
# INFRA_FAULT deliberately DOMINATES ROLLBACK. That precedence IS the asymmetry:
# a missing sentinel can only ever take a rollback away, never add one.
#
# A human-readable per-job line is appended to the file named by
# $RAN_SENTINEL_REPORT (default /dev/null) so the caller — not this library —
# owns stdout and the ::error::/::notice:: annotations.
ran_sentinel_decide() {
  if [ "$#" -eq 0 ] || [ "$(($# % 4))" -ne 0 ]; then
    echo "::error::ran_sentinel_decide: expects 4-tuples of <job> <result> <begin> <end>; got $# argument(s)" >&2
    return 2
  fi

  local report="${RAN_SENTINEL_REPORT-/dev/null}"
  : >"$report" || return 2

  local infra=0 rollback=0
  local job result begin end verdict
  while [ "$#" -gt 0 ]; do
    job="$1"
    result="$2"
    begin="$3"
    end="$4"
    shift 4
    verdict="$(ran_sentinel_classify "$job" "$result" "$begin" "$end")" || return 2
    printf '  %-24s result=%-10s verdict=%s\n' "$job" "$result" "$verdict" >>"$report"
    case "$verdict" in
      ran-ok) ;;
      not-dispatched | ran-failed | ran-failed-partial) rollback=1 ;;
      phantom | phantom-green) infra=1 ;;
      *)
        echo "::error::ran_sentinel_decide: unrecognised verdict '${verdict}' for job '${job}'; failing closed." >&2
        return 2
        ;;
    esac
  done

  if [ "$infra" -eq 1 ]; then
    printf 'INFRA_FAULT'
  elif [ "$rollback" -eq 1 ]; then
    printf 'ROLLBACK'
  else
    printf 'NO_ROLLBACK'
  fi
}
