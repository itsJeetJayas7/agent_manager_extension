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
                     ├──► PAUSED (queue paused, scope INCLUDE_ACTIVE)
                     ├──► CANCEL_REQUESTED ──► CANCELLED                  │    │
                     ├──► FAILED ──► (retry: QUEUED)                      │    │
                     ├──► BLOCKED (marker)                                │    │
                     ├──► NEEDS_ATTENTION ──► (continue/retry/skip)       │    │
                     ├──► ORPHANED (daemon restart mid-run)               │    │
                     └──► SKIPPED (explicit only)                         │    │
                                             INTEGRATING ──► AWAITING_INTEGRATION
```

## PAUSED

A run reaches `PAUSED` only from an `INCLUDE_ACTIVE` queue pause: the adapter
is asked to stop at a safe point and the native session is left intact. It is
not a failure state and carries no error. When the queue resumes, every
`PAUSED` run is continued in the *same* session (`ContinueRun`), or requeued
if it never reached the provider. A `PAUSED` run does not hold a slot.

## ORPHANED and restart recovery

`ORPHANED` is reachable from every dispatched state, because it is what a run
*is* after the daemon dies under it. On startup:

- a run with a known `ProviderSessionID` whose adapter can resume is marked
  `ORPHANED` and then reattached (`ORPHANED → AUTO_RESUMING → RUNNING`) in the
  same native session;
- a run with no session goes to `NEEDS_ATTENTION`;
- a `CANCEL_REQUESTED` run settles as `CANCELLED`, so it cannot hold a slot.

`CANCEL_REQUESTED` is also reaped by a 60-second watchdog when no adapter
execution is live: otherwise nothing would ever emit `TurnInterrupted` for it
and the slot would be lost until the next restart.

## Terminal states

`COMPLETED`, `BLOCKED`, `NEEDS_ATTENTION`, `FAILED`, `CANCELLED`, `SKIPPED`,
`ORPHANED`. `COMPLETED` can transition back to `RUNNING` via an explicit
user-ordered continuation, and to `INTEGRATING` so a batch can be re-integrated
after a manual fix.

`AUTO_RESUMING` is a transient pre-dispatch marker, never a dispatched state:
dispatch always moves a run to `RUNNING` (session known) or `STARTING`.

## Completion gate

Managed runs only (identified by `AGENT_CONDUCTOR_RUN_ID` in the provider
child environment): the daemon's Stop hook blocks the first stop attempt that
lacks `<AC_RESULT>…</AC_RESULT>` and instructs the agent to finish properly.
If the second stop (flagged `stop_hook_active`) still lacks a marker, the run
is marked `NEEDS_ATTENTION` — never an infinite loop. External sessions are
never gated.

## Queue holds

Dispatch is held — and only dispatch — by `PauseState`. Quota resolution, the
cancellation watchdog, hook processing and the pause timer all keep running
while the queue is paused. A hold records its source (`user`, `quota`,
`external-agent`, `schedule`), so the UI always has one honest answer to "why
is nothing running?". A user pause outranks every system hold and is cleared
only by the user or by its own timer.

## Quota waits

`RATE_LIMIT` failures record every exhausted window (five-hour and weekly).
`nextEligibleAt = max(reset time of every exhausted window)`. The same native
session/thread is resumed after eligibility; if the turn never meaningfully
began, the original effective prompt is retried instead. Unknown reset times
are surfaced as unknown — never invented.
