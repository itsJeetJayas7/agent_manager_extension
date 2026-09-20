# Run State Machine

All transitions are validated in `daemon/internal/model` and persisted with an
audit event (`STATE_CHANGED` plus a semantic event) inside one transaction.

```text
DRAFT ───────────► QUEUED ──────────► WAITING_FOR_SLOT ───┐
                     │                                   │
                     ├──────────► WAITING_FOR_EXTERNAL ───┤
                     │                                   ▼
                     │            ┌─────────────────── PREPARING
                     │            │                       │
                     │            ▼                       ▼
                     │          STARTING ─────────────► RUNNING ◄───────────────┐
                     │            │                     │  ▲                    │
                     │            │ quota before start  │  │ auto-resume        │
                     │            ▼                     ▼  │                    │
                     │      WAITING_FOR_QUOTA ◄──── AUTO_RESUMING                │
                     │            │                     │                       │
                     │            │ reset              ▼                       │
                     │            └──────────────► VERIFYING ──► COMPLETED      │
                     │                                  │                  ▲    │
                     │                     fix loop     │ manual continue   │    │
                     │                     └► RUNNING ──┘                   │    │
                     │                                                   │    │
                     ├──► CANCEL_REQUESTED ──► CANCELLED                  │    │
                     ├──► FAILED ──► (retry: QUEUED)                      │    │
                     ├──► BLOCKED (marker)                                │    │
                     ├──► NEEDS_ATTENTION ──► (continue/retry/skip)       │    │
                     ├──► ORPHANED (daemon restart mid-run)               │    │
                     └──► SKIPPED (explicit only)                         │    │
                                             INTEGRATING ──► AWAITING_INTEGRATION
```

## Terminal states

`COMPLETED`, `BLOCKED`, `NEEDS_ATTENTION`, `FAILED`, `CANCELLED`, `SKIPPED`,
`ORPHANED`. COMPLETED can transition back to RUNNING via an explicit
user-ordered continuation.

## Completion gate

Managed runs only (identified by `AGENT_CONDUCTOR_RUN_ID` in the provider
child environment): the daemon's Stop hook blocks the first stop attempt that
lacks `<AC_RESULT>…</AC_RESULT>` and instructs the agent to finish properly.
If the second stop (flagged `stop_hook_active`) still lacks a marker, the run
is marked `NEEDS_ATTENTION` — never an infinite loop. External sessions are
never gated.

## Quota waits

`RATE_LIMIT` failures record every exhausted window (five-hour and weekly).
`nextEligibleAt = max(reset time of every exhausted window)`. The same native
session/thread is resumed after eligibility; if the turn never meaningfully
began, the original effective prompt is retried instead. Unknown reset times
are surfaced as unknown — never invented.
