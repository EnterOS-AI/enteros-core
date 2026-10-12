---
title: "Audit Ledger Design: HMAC Chains and a Historical Verification Fix"
date: 2026-04-21
slug: audit-chain-verification
description: "The historical HMAC-linked audit-event design and the verification panic fix that shipped in PR #1339."
tags: [security, audit, HMAC, enterprise, compliance]
---

# Audit Ledger Design: HMAC Chains and a Historical Verification Fix

> **Current status (July 14, 2026):** Core still contains the `audit_events`
> schema and authenticated read/verifier endpoint, but the in-Core Python
> producer described by this article was removed during the workspace-runtime
> split. The active standalone runtime and templates do not currently write
> these rows. This article documents the historical design and verification
> fix; it is not a claim that current agent work is being recorded in this
> ledger. The restore-or-retire decision is tracked internally.

The April 2026 in-Core runtime implementation wrote selected agent execution
events to an HMAC-SHA256-linked audit table so changes to stored entries could
be detected during verification.

This post explains how that system works and what changed in PR #1339.

---

## The problem with plain audit logs

A standard audit log is a list of events with timestamps. It's useful for debugging, but it has a structural weakness: nothing stops someone with database access from editing past rows. A malicious actor — or a buggy cleanup script — can remove or modify entries, and the log looks perfectly fine.

For production multi-agent systems, that matters. Your compliance team needs to know: *did that agent actually call the API it was supposed to call, or did it skip the approval step?* A plain log can't answer that with confidence.

The audit-ledger design was built to answer that question.

---

## HMAC-SHA256 chain architecture

The historical producer treated the ledger as an **append-only, HMAC-linked
log**. Each entry contains:

- The event data (who did what, when, what the result was)
- An HMAC-SHA256 of the current entry, signed with a server-side secret
- The HMAC of the *previous* entry embedded as part of the signing context

This creates a chain — like a blockchain, but not distributed. Every entry's HMAC depends on the previous entry's HMAC, which depends on the one before that, and so on back to the genesis entry.

```
Entry 0: HMAC₀ = HMAC(genesis_payload + genesis_secret)
Entry 1: HMAC₁ = HMAC(event₁ + HMAC₀ + secret)
Entry 2: HMAC₂ = HMAC(event₂ + HMAC₁ + secret)
...
```

If you change *any* past entry, its HMAC changes. That breaks the chain at the next verification step. The tampered entry is detectable.

---

## Verifying the chain

`verifyAuditChain` walks the supplied rows in chronological order, recomputing
each HMAC and comparing it against the stored value. The API only returns a
verdict when the query can represent a complete chain prefix.

If an entry fails to verify, the function returns `false`. A caller can use
that verdict to alert or log the discrepancy. A `true` verdict only covers the
rows the endpoint was able to verify; it does not prove that all expected
agent actions were emitted.

With an owned producer and signing-key contract, this design can make persisted
rows tamper-evident. It cannot prove that every expected action was emitted.

---

## The bug PR #1339 fixed

In Go, slicing a string beyond its length causes a panic:

```go
// This panics if len(ev.HMAC) < 12
log.Printf("expected: %q  got: %q", ev.HMAC[:12], expected[:12])
```

`verifyAuditChain` was using `[:12]` to truncate HMACs for log readability — 12 characters is enough to identify a key without printing the full hash. But if an audit row had been corrupted (a database write failure, a migration bug, manual intervention), the stored HMAC could be shorter than 12 bytes. When that row was processed, the verification pass would panic and crash.

A tamper attempt wouldn't just fail verification — it would take down the verification process.

**The fix (PR #1339):** add a length check before truncation.

```go
storedPrefix := ev.HMAC
if len(storedPrefix) > 12 {
    storedPrefix = storedPrefix[:12]
}
computedPrefix := expected
if len(computedPrefix) > 12 {
    computedPrefix = computedPrefix[:12]
}
log.Printf("expected: %q  got: %q", storedPrefix, computedPrefix)
```

The logic is unchanged — if the HMAC is long enough, the same 12-char prefix is logged. If it's short or missing, a shorter prefix (or none) is logged. Either way, the chain verification still runs, and mismatches still fail correctly.

The panic is gone. The integrity guarantee holds.

---

## What the fix guarantees

PR #1339 hardens the verification function for rows that already exist:
short or corrupt HMAC strings produce a failed verdict instead of crashing the
verification process. It does not provide or validate a current event producer.

---

## Next steps

- [Org-scoped API keys guide](/docs/guides/org-api-keys) — mint your first named key
- [Architecture: Org API Keys](/docs/architecture/org-api-keys) — the full design
- [Platform API contract](../../api-protocol/platform-api.md) — current read-endpoint fields and pagination

---

*HMAC-SHA256 audit ledger shipped in PR #594. HMAC truncation guard shipped in PR #1339. Org-scoped API keys shipped in PRs #1105, #1107, #1109, #1110.*
