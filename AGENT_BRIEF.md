# Agent Conductor — Remediation & Elevation Brief

**You are the next coding agent on this repository.** This document is your complete
work order. It is the product of a full line-by-line audit of the existing codebase
(written by a previous agent) plus a feature and design mandate from the repository
owner. Everything below has been verified against the actual source — file paths and
line numbers are real at the time of writing, but **re-verify before editing**, because
line numbers drift.

---

## 0. Ground rules

1. **The repo is not a git repository.** `git init`, commit the current state as a
   baseline (`chore: baseline before remediation`), then work in logical commits. This
   is non-negotiable — several of the changes below are destructive rewrites and the
   owner needs a rollback point.
2. **Current state:** `go build ./...`, `go test ./...`, `tsc --noEmit` (both projects),
   and `vitest` all pass. **This means nothing.** The extension is dead on arrival at
   runtime (§1), the Codex adapter deadlocks on first use (§4.2), and the daemon has a
   guaranteed-fatal data race (§2.1). The test suite tests the wrong things.
3. **Run `go test -race ./...` before and after every daemon change.** It currently
   FAILS (`TestWeeklyExhaustionSuppressesAnchor`). It must pass when you are done.
4. **Do not "fix" by deleting features.** Every capability described in `README.md`
   must still work — better — when you finish. If you find a claim in the README that
   the code cannot honestly support, fix the code, or fix the README and say so
   explicitly in your report.
5. **Verify provider API surfaces against the actually-installed CLIs.** Several
   integration points (Codex `app-server` JSON-RPC method names, Claude Code hook
   event names and flags) were written from memory or documentation and may not match
   the installed binaries. Run `codex app-server` and `claude --help` and check. Where
   reality differs from this brief, reality wins — and record the divergence in
   `docs/provider-capabilities.md`.
6. **Report honestly.** If something in here turns out to be wrong, or you cannot
   complete a section, say so plainly in your final summary rather than silently
   dropping it.

### Orientation

```
src/extension/        VS Code extension host (TypeScript, esbuild → dist/extension.js)
webview/src/          React 18 sidebar + panels (Vite → dist/webview/webview.js)
daemon/               Go 1.27 persistent daemon (the thing that actually runs agents)
  internal/api/       Unix-socket NDJSON protocol v1, token auth
  internal/scheduler/ the brain: queue, batches, quota waits, anchors, staging
  internal/providers/ Claude (PTY) + Codex (app-server JSON-RPC) + Fake adapters
  internal/db/        SQLite WAL, migrations, state-machine transactions
  internal/hooks/     writes provider hook config (~/.claude/settings.json, ~/.codex/config.toml)
  internal/hooksbridge/  the `agent-conductor hook <provider>` subcommand
  internal/worktrees/ git worktree isolation for PARALLEL batch rows
  internal/vault/     AES-256-GCM at-rest encryption
scripts/, esbuild.mjs, vite.config.ts   build pipeline → .vsix
```

---

## 1. P0 — The extension does not work at all

These three bugs together mean a user who installs the VSIX sees a blank, unstyled
sidebar with a permanently offline daemon. Fix these first and confirm by actually
installing the VSIX and opening it.

### 1.1 `acquireVscodeApi` is a typo — the webview throws before React mounts

`src/extension/extension.ts:201`

```html
<script nonce="${nonce}">window.__acVscode=acquireVscodeApi();</script>
```

The real global is **`acquireVsCodeApi`** (capital C). This throws `ReferenceError`,
the inline script dies, `window.__acVscode` is `undefined`, and every
`vscodeApi.postMessage` in `webview/src/api/client.ts` silently no-ops. The sidebar
renders an empty shell that never talks to anything.

Fix the spelling. Then **guard it**: `acquireVsCodeApi()` can only be called once per
webview document, so wrap it:

```js
window.__acVscode = window.__acVscode || acquireVsCodeApi();
```

Add a webview test that asserts the generated HTML contains `acquireVsCodeApi(` — this
class of typo must never ship again.

### 1.2 The stylesheet is built but never loaded

`vite.config.ts` emits CSS to `dist/webview/assets/main.css` (confirmed present in
`agent-conductor-0.1.0.vsix`), but `webviewHtml()` in `extension.ts:183-205` only
injects `<script src=webview.js>`. There is no `<link rel="stylesheet">`. Every single
class in `webview/src/styles/global.css` — the entire design system — is inert.

Two acceptable fixes; pick **B**:

- **A.** Add `<link rel="stylesheet" href="${cssUri}" nonce="${nonce}">` and tighten
  CSP to `style-src ${webview.cspSource}`.
- **B (preferred).** Configure Vite to inline CSS into the JS bundle
  (`build.cssCodeSplit: false` plus a small plugin, or simply import the CSS as a
  string and inject a `<style>` element at boot). One artifact, no CSP juggling, no
  flash of unstyled content. Since you are rewriting the design system anyway (§6),
  this is the cleaner base.

Either way, **remove `style-src 'unsafe-inline'`** from the CSP once inline styles are
no longer required, or keep it only if §6's implementation genuinely needs it.

### 1.3 The daemon binary is never found

`src/extension/configuration.ts:26-27` looks for:

```
<extensionPath>/bin/agent-conductor
<extensionPath>/bin/agent-conductor-darwin-arm64
```

`scripts/build.mjs` places it at `dist/bin/agent-conductor`, and the VSIX ships it at
`extension/dist/bin/agent-conductor`. The path is wrong, so `resolveDaemonBinary()`
falls through to `which agent-conductor` and returns `""` for anyone who hasn't
installed the service. Result: `{type:"daemonOffline", reason:"no-binary"}` forever.

Fix:

- Resolve `dist/bin/agent-conductor` first, and keep `dist/bin/agent-conductor-<os>-<arch>`
  as a second candidate.
- **`chmod 0o755` the binary on activation if it is not executable.** VSIX extraction
  does not reliably preserve the execute bit; this has bitten every extension that
  ships a native helper.
- Validate the binary before using it: run `<bin> version` with a 5s timeout and check
  the output starts with `agent-conductor`. Surface a real error in the UI if it fails
  (wrong architecture, quarantined by Gatekeeper, etc.) instead of silently reporting
  "offline".
- On macOS, detect the `com.apple.quarantine` xattr and tell the user exactly what to
  run (`xattr -d com.apple.quarantine <path>`) rather than failing opaquely.

### 1.4 Also in the extension host, fix while you are here

| Location | Problem | Fix |
|---|---|---|
| `extension.ts:141` | `registerWebviewViewProvider` return value discarded | push to `context.subscriptions` |
| `extension.ts:185` | `Math.random().toString(36)[2]` can yield `undefined`; not crypto-random | `require("crypto").randomBytes(16).toString("base64")` |
| `extension.ts:121` | `$(agent)` is not a real codicon — renders as literal text | use `$(pulse)` / `$(circle-filled)` / `$(debug-pause)` per state (see §5.6) |
| `extension.ts:167` | panels lack `retainContextWhenHidden` | add it, or make panels fully re-hydrate on `visible` |
| `extension.ts:174,179` | `postMessage` fires before the webview script has loaded — `detailBatchId` is dropped | webview must send a `ready` message on mount; host queues and flushes on `ready` |
| `extension.ts` (`onConnected`) | `client.onClose(...)` is never wired | wire it to `scheduleReconnect()` with exponential backoff (1s → 30s cap) so a daemon crash self-heals instead of bricking the UI until reload |
| `daemonClient.ts:82-92` | `onDead` registered on both `close` and `error` → fires twice | guard with a `dead` flag |
| `daemonClient.ts:129` | per-request 30s `setTimeout` never cleared on resolve | clear it; also make the timeout configurable and shorter (10s) for interactive calls |
| `daemonClient.ts` | no protocol-version check on `hello` response | reject mismatches with a clear "update the extension / daemon" message |
| `configuration.ts:52` | `detectProviderRuntimes()` is exported and never called | wire it into onboarding (§7.1) or delete it |

---

## 2. P0 — Daemon crashes and deadlocks

### 2.1 `quota.Guardian` has no mutex — guaranteed fatal crash

`daemon/internal/quota/quota.go`. `Guardian.windows` is a plain `map[string][]QuotaWindow`
read and written from at least four goroutines: the 1 Hz scheduler loop, provider event
callbacks, the API server's `snapshot()`/`quotaState()` handlers, and `main.restoreQuota`.

`go test -race ./...` reports this today (`quota.go:28`, `:30`, `:66` vs
`anchor.go:143,172,179`, `schedule.go:59`). In production this is not a warning — it is
`fatal error: concurrent map read and map write`, which is **unrecoverable** and kills
the daemon mid-run.

Fix: add a `sync.RWMutex` to `Guardian`; `Observe` takes the write lock, `Blocked` and
`Windows` take the read lock. **`Windows()` must return a defensive copy** — it
currently hands out the live slice, which callers then range over outside any lock.

While you are in this file: `Blocked()` has a dead `any` variable (declared, assigned,
discarded via `_ = any`). Remove it. `any` is also a predeclared type alias in modern
Go — shadowing it is confusing.

### 2.2 `Conductor.clock` is read and written without synchronisation

`scheduler.go` — `SetClock` writes `c.clock`, `Now()` reads it from every goroutine.
Only tests call `SetClock`, but it is still a race. Store it in an `atomic.Pointer` or
guard it with the existing `settingsMu`.

### 2.3 Window Anchor: send on closed channel → panic

`daemon/internal/scheduler/anchor.go:238-253`

```go
go func() { adapter.Execute(ctx, spec, func(ev providers.Event) {
        select { case done <- ev: default: }   // producer
}) }()
go func() { for ev := range done {
        if ev.Kind == providers.EvTurnCompleted { ...; close(done); return }   // consumer closes
} }()
```

The producer goroutine is still live when the consumer closes `done`. Any event emitted
after `EvTurnCompleted` (Codex emits `account/rateLimits/updated` freely) panics the
daemon with `send on closed channel`.

Fix: never close a channel from the receiving side. Use a `context` + `sync.Once`, or
have `Execute` return and then close, or just drop the close entirely and let the
channel be garbage-collected.

Also in `tryAnchor`:
- The anchor slot is **claimed before checking the adapter exists** (`ClaimAnchor` at
  :219, `registry.Get` at :222). If the adapter is missing, the window is permanently
  burned and no anchor will ever fire for it. Check first, claim second.
- `pickAnchorModel` (:278) calls `containsAny(lower, want)` where `lower` is
  `m.ID + " " + m.Name` **not lowercased**, matched against lowercase needles
  (`"haiku"`, `"mini"`, …). Model names are usually title-cased, so this never matches
  and always falls through to `caps.Models[0]` — which may be the most expensive model.
  `strings.ToLower` it. The same case-sensitivity bug is in `onHookPayload` (:63) and
  `claudeQuotaFromNotification` (:82).

### 2.4 API server: send on closed channel + goroutine leak

`daemon/internal/api/api.go`

- `Close()` (:161) does `close(cc.out)` for every client while the per-connection
  subscription goroutine (:253) is still calling `s.send(out, ...)`. Same panic class
  as §2.3. Use a per-client `done` channel and select on it.
- The subscription goroutine spawned at `case "subscribe"` (:250) **never terminates**
  and **never calls `conductor.Unsubscribe(ch)`**. Every extension-host reconnect leaks
  one goroutine and one 256-slot channel that the conductor keeps fanning out to
  forever. Tie its lifetime to the connection; unsubscribe in the `handle()` defer.
- `subscribe` can be called repeatedly on one connection, multiplying the leak. Make it
  idempotent per connection.
- `handle()`'s `out` channel is never closed on normal disconnect — the writer
  goroutine `for msg := range out` blocks forever.
- `diagnostics()` (:673) calls `a.Capabilities(contextBG())` with **no timeout**. A
  hung `claude --help` or `codex app-server` wedges the diagnostics command
  indefinitely. Use `context.WithTimeout(…, 15*time.Second)` like `refreshCapabilities`
  does.
- Token comparison is `!=` on strings. Use `crypto/subtle.ConstantTimeCompare`. Low
  severity on a 0600 Unix socket, but it is one line.
- `snapshot()` contains a declared-and-unused `type runOut struct`. Delete it.

### 2.5 `snapshot()` is an N+1 query storm on a 1 Hz path

`api.go` `snapshot()` calls `ListBatches(100)` then, per batch, `ListRunsByBatch` +
`ListRows` — 201 SQLite queries per snapshot. The webview polls every 15s
(`store.ts`), **and** re-requests a full snapshot on every single daemon event
(`daemonEvent` → `requestSnapshot()`), and every window focus change. Meanwhile
`schedule()` runs every second and itself does `ListBatches(500)` plus a `GetBatch` per
active run.

This is the single largest performance defect. Fix it structurally:

1. Add composite queries: one `SELECT … FROM runs WHERE batch_id IN (…)` and one for
   rows, joined in memory. Or simply `SELECT * FROM runs ORDER BY sort_key` once and
   bucket by `batch_id`.
2. Cache the snapshot with a short TTL (250ms) and a dirty flag set by `emit()`.
3. **Replace full-snapshot-on-every-event with incremental patches.** The daemon
   already emits `runUpdated`/`batchUpdated` with an ID — send the changed entity, and
   have the webview merge it. Keep the full snapshot for initial load and a slow
   (60s) reconciliation tick.
4. In `schedule()`, hoist the `ListBatches` and the active-run scan out of the per-batch
   loop; the `GetBatch(r.BatchID)` inside the counting loop (`schedule.go:47`) is
   especially wasteful — join or cache it.
5. Stop scheduling work every second when there is nothing to do. Event-driven `Poke()`
   already exists; the ticker should be a slow safety net (5–10s), with a computed
   next-wake time when a quota deadline or pause timer is pending.

---

## 3. P0/P1 — Scheduler and queue logic bugs

### 3.1 "Run Next" puts the run LAST

`daemon/internal/scheduler/commands.go:76-79`

```go
sortKey, err := c.store.NextSortKey()      // MAX(sort_key)+1  → small ints: 1,2,3…
if in.RunNext {
    sortKey = time.Now().UnixNano()        // ≈ 1.8e18
}
```

`NextSortKey()` returns small monotonically increasing integers. `UnixNano()` is
astronomically larger. Runs dispatch `ORDER BY sort_key`, so **`Run Next` sends the run
to the back of the queue** — the exact opposite of what the button says and what
`README.md` promises. `ReorderQueue` (:379) has the same inversion for "move to end".

Fix properly: adopt a single coherent ordering scheme. Use a sparse integer sort key
(allocate in steps of 1024 so insertions between neighbours need no rewrite), or a
fractional/LexoRank key. `RunNext` must compute
`min(sort_key of all QUEUED runs) - gap`, and `ReorderQueue(runID, "")` must compute
`max(...) + gap`. Add a renormalisation pass when gaps are exhausted.

**Add a test** that queues three runs, calls `enqueueRun{runNext:true}`, and asserts the
dispatch order.

### 3.2 Cancelling a queued run permanently consumes a slot

`CancelRun` (`commands.go`) transitions any non-terminal run to `CANCEL_REQUESTED`.
`CANCEL_REQUESTED` is in `activeStates()` (`schedule.go`), so it counts against
`MaxTotal`/`MaxClaude`/`MaxCodex`. For a run that was never dispatched there is no
provider turn in flight, so nothing ever emits `TurnInterrupted`, so nothing ever moves
it to `CANCELLED`. **The run sits in `CANCEL_REQUESTED` forever, and the queue loses a
slot permanently.** Cancel two queued runs with default `MaxTotal: 2` and the queue is
dead until the daemon restarts.

Fix:
- If the run has no active execution (no entry in `c.cancels`, no `ProviderSessionID`,
  state is `QUEUED`/`WAITING_*`), transition **directly to `CANCELLED`**.
- Add a watchdog in `schedule()`: any run in `CANCEL_REQUESTED` for > 60s with no live
  adapter execution is forced to `CANCELLED` with an event explaining why.
- Add a scenario test for both paths.

### 3.3 "Delete Shared Testing & Sudo Block" does not delete anything

`commands.go:437`

```go
func (c *Conductor) DeleteSecretBlock() error {
    return c.store.SetSetting("secretBlock.deleted", true)
}
```

Nothing ever reads `secretBlock.deleted`. `latestSecretBlock()` still returns the
plaintext, `RevealSecretBlock()` still reveals it, and queued runs still attach it. The
user presses Delete on their sudo password, gets no error, and the secret is still
being shipped to the provider on the next run. **This is the most serious
trust/security defect in the codebase.**

Fix: actually delete. `DELETE FROM secret_versions`, zero the row, clear the redactor
entry, emit `settingsUpdated`, and make `latestSecretBlock()` return empty. Add a
confirmation in the UI (§6) and a test asserting that after delete, a newly-enqueued
run with `attachSecrets:true` produces an effective prompt with no secret section.

Related: `UpdateSecretBlock` never clears a previously-set deleted flag, and
`RevealSecretBlock` is called on **every `Composer` mount** in `QueueView.tsx`
(`useEffect(() => { rpc("revealSecretBlock") … }, [])`) purely to test existence —
decrypting a secret and sending it over the socket to answer a boolean. Replace with a
`hasSecretBlock: boolean` field in the snapshot.

### 3.4 `newID()` will collide

`commands.go:575`

```go
func newID(prefix string) string {
    return fmt.Sprintf("%s_%s", prefix, fmt.Sprintf("%d%d", time.Now().UnixNano(), time.Now().Unix()%1000))
}
```

Two calls within the same clock tick produce identical IDs. `ApplyBatch` creates rows
and runs in a tight loop; on platforms with coarse monotonic-clock granularity this
collides and violates the `PRIMARY KEY`, aborting batch creation halfway with rows
already inserted (no transaction wraps it). Use `crypto/rand` (16 bytes, base32/hex) or
a ULID. **Also wrap `ApplyBatch`'s inserts in a single DB transaction** so a failure
does not leave a half-created batch.

### 3.5 `ContinueRun` acts on stale state and ignores failure

`commands.go` `ContinueRun`:
- `TransitionRun(...)` return value is discarded. From `COMPLETED`, the only legal
  transition is `→ RUNNING`, so `→ AUTO_RESUMING` silently fails — and then
  `launchExecution` runs anyway, against a run whose persisted state does not match.
- It passes the pre-transition `*model.Run` into `launchExecution`, which calls
  `pickActiveState(r)` on stale data.

Fix: check every `TransitionRun` error in the scheduler (there are ~15 discarded ones —
audit them all), and re-read the run after transitioning before dispatch.
`scheduler/exec.go` has the same pattern at the `pickActiveState` call.

### 3.6 `UpdateRun` wipes fields with empty input

`commands.go`: `r.Model, r.Effort, r.Permission = in.Model, in.Effort, in.Permission`
unconditionally clobbers, then conditionally restores only `Permission`. A partial
update that omits `model` blanks the model. Make it a true patch (pointer fields or an
explicit field mask).

### 3.7 `CopyBatchReport` panics on an empty provider string

`commands.go:517`: `strings.ToUpper(r.Provider[:1])` — index out of range if
`Provider == ""`. Guard it. (Use `golang.org/x/text/cases` or a two-line helper.)

### 3.8 Other scheduler issues

| Location | Problem |
|---|---|
| `schedule.go` | When `Paused`, `schedule()` returns immediately, which also skips `resumeQuotaReady`. Intentional, but see §5 — the new pause model needs this restructured. |
| `batch.go` `advanceBatch` | A row with zero runs stalls the batch forever (`rowDone` returns false for an empty slice, `rowAllStarted` too, so the loop `return nil`s each tick). Reject empty rows at creation and skip them defensively. |
| `batch.go` `integrateRow` | Dead `for _, r2 := range rowRuns { _ = r2 }` loop. Delete. |
| `batch.go` `batchNumber` | Hashes the batch ID into `1..89`. **Collisions are near-certain** and drive git branch names (§4.4). Store a real monotonic `batch_number` column instead. |
| `batch.go` `prepareCardWorktrees` | Branch names mix 1-indexed rows (`row.Position+1`) with 0-indexed cards (`i`). Pick one. |
| `exec.go` `onProviderEvent` | Does a full `store.GetRun()` **and** an `emitRun()` for *every* event including every `EvMessageDelta`. For Codex streaming that is a DB round-trip per token delta. Batch/throttle delta handling; only persist on meaningful state change. |
| `exec.go` `truncate`, `model.go` `PreviewRedacted` | Byte-slice truncation splits UTF-8 runes → mojibake in the UI. Truncate by rune. |
| `exec.go` `launchExecution` | `case promptOverride != "": … case isContinue: effective = promptOverride` — the second case is unreachable dead code. |
| `exec.go` `beginVerification` | 30-minute hard timeout on verification commands with no way to configure it, and no way to surface progress. Make it a setting; stream progress to the UI. |
| `db.go` `ListBatches` | `ORDER BY created_at DESC` on `RFC3339Nano` **strings**. `RFC3339Nano` strips trailing zeros, so `…:00.5Z` sorts after `…:00.45Z` lexicographically but before it chronologically. Store an integer epoch-nanos column, or format with fixed-width fractional seconds (`.000000000`). |
| `db.go` `ListRunEvents` | `ORDER BY id LIMIT ?` returns the **oldest** N events. The timeline UI wants the newest. Use a subquery or `ORDER BY id DESC LIMIT ?` reversed in Go. |
| `db.go` `CreateRun` | `strings.Repeat("?,", 41)+"?"` — a hand-counted placeholder count that will silently break the moment a column is added. Build the placeholder list from the column list programmatically. |
| `db.go` | No retention policy. `run_events` grows without bound. Add a `VACUUM`/prune job (keep last N events per run, drop events for runs terminal > 30 days) and a `Compact()` maintenance RPC. |
| `db.go` `TransitionRun` | Does not distinguish `sql.ErrNoRows` from real errors. |
| `model.go` transitions | `COMPLETED → RUNNING` is allowed (manual continue) but `COMPLETED → INTEGRATING` is not, which blocks re-integration after a manual fix. Audit the table against `docs/state-machine.md` — they have drifted. |

---

## 4. P0/P1 — Provider adapters

### 4.1 Claude adapter: replace TUI puppeteering with the headless protocol

`daemon/internal/providers/claude.go` drives Claude Code by starting the **interactive
TUI in a PTY**, sleeping 2500ms, and writing a bracketed-paste escape sequence followed
by `\r`:

```go
select { case <-time.After(2500 * time.Millisecond): ... }
paste := "\x1b[200~" + spec.Prompt + "\x1b[201~\r"
_, werr := ptmx.Write([]byte(paste))
```

This is the most fragile thing in the codebase. It breaks on: slow startup on cold
cache, the trust dialog, first-run theme/onboarding prompts, any TUI layout change, any
prompt containing sequences the TUI intercepts, and terminal resize. It is also
unobservable — PTY output is explicitly *not* parsed ("fragile"), so the adapter is
blind and depends entirely on hooks for state.

**Rewrite it against Claude Code's documented programmatic interface:**
`claude -p` / `--print` with `--output-format stream-json` and
`--input-format stream-json`, plus `--resume <session-id>` for continuation and
`--session-id <uuid>` for deterministic identity. You get structured, parseable events:
turn start, tool use, assistant message deltas, result with a stop reason. That is a
real protocol instead of screen-scraping.

Check `claude --help` on the installed binary for the exact flag set before you commit
to this — if stream-json is unavailable in the installed version, keep the PTY path as
a **fallback** behind a capability probe, but make the structured path the default.

Regardless of which path survives, these bugs must be fixed:

- **No `cmd.Wait()` anywhere.** `pty.Start` starts the process; nothing reaps it.
  Every managed Claude run leaves a zombie until the daemon exits. Call `cmd.Wait()`
  in a goroutine and surface the exit code.
- **`meaningful` progress detection is broken.** The handler branches on
  `"PostToolUse"`, `"PreToolUse"`, etc., but `managedSettingsJSON()` **never registers
  those hooks**. `MeaningfulProgress` is therefore only ever set from a non-empty
  `Stop.last_assistant_message` — which means the "did the agent actually start work?"
  decision that drives quota-resume-vs-restart is wrong most of the time. Either
  register the tool hooks or derive progress from the stream-json events.
- **Dead branch:** `paste := …; if spec.Continue { paste = … }` — both branches are
  identical.
- **Subscription protection is incomplete.** `claudeChildEnv` strips
  `ANTHROPIC_API_KEY` only. Also strip `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  `ANTHROPIC_MODEL`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, and
  `AWS_*`/`GOOGLE_*` credentials that would redirect billing. Also: the first branch
  `if kv == "ANTHROPIC_API_KEY"` is dead — `os.Environ()` entries always contain `=`.
- `deadline := time.NewTimer(24 * time.Hour)` is never stopped — one leaked timer per
  run. Also make the deadline configurable.
- `Cancel()`'s 5-second delayed goroutine closes `c.running[runID].ptty` — if a retry
  registered a *new* run under the same ID inside that window, it kills the new run.
  Capture the specific `*claudeRun` pointer instead of re-looking-up by ID.
- `Capabilities()` hardcodes `opus`/`sonnet`/`haiku`. Probe the installed CLI for the
  real model list where possible, and cache it.

### 4.2 Codex adapter: guaranteed deadlock on first use

`daemon/internal/providers/codex.go`

```go
func (c *Codex) ensureStarted(ctx context.Context) error {
    c.mu.Lock()
    defer c.mu.Unlock()          // ← holds c.mu for the whole function
    ...
    _, err = c.call(ctx2, "initialize", …)   // ← call() starts with c.mu.Lock()
}

func (c *Codex) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
    c.mu.Lock()                  // ← blocks forever; sync.Mutex is not reentrant
```

`ensureStarted` calls `call("initialize")` while holding `c.mu`; `call` immediately
tries to take `c.mu`. **This is an unconditional deadlock the first time anything
touches Codex**, and because `ensureStarted` never returns, `c.mu` is held forever —
every subsequent Codex operation, including `Capabilities()` from the snapshot path,
blocks permanently. The 15s context timeout does not help: the goroutine is parked on
the mutex, not in a select.

This is never hit by the test suite (the fake provider is used throughout), which is
why it shipped.

Fix: split the locking. Have `ensureStarted` hold the lock only for process spawn and
state mutation, release it, then perform the `initialize` handshake. Or introduce an
unlocked `callLocked`/`doCall` used internally. Add a regression test that exercises
`ensureStarted → call` against a stub stdio server.

Also in this file:

- **`Cancel(runID)` is a complete no-op.** `c.cancels` is declared, initialised, and
  **never written to**. The map lookup always returns nil. Cancelling a Codex run does
  nothing. It must send `turn/interrupt` for the run's thread (`InterruptThread` already
  exists and is never called) *and* cancel the execution context. Track
  `runID → threadID` when `Execute` starts.
- No `cmd.Wait()` on the `codex app-server` child → zombie.
- `failAllPending` sets `started = false` but leaves the dead child process around; the
  next `call` re-enters `ensureStarted` (and, before the fix above, deadlocks). Add
  proper restart-with-backoff and surface "Codex app-server crashed" to the UI.
- `parseEfforts(defaultEffort string)` ignores its parameter entirely and returns a
  fixed list. Either use the parameter or drop it.
- `codexEnv()` is `return os.Environ()` with a comment claiming it preserves
  `CODEX_HOME`. It is a no-op function. Delete it or make it do something.
- **Verify every JSON-RPC method name against the installed `codex app-server`.**
  `account/read`, `model/list`, `thread/start`, `thread/resume`, `turn/start`,
  `turn/interrupt`, `account/rateLimits/read`, and the notification names
  (`item/agentMessage/delta`, `turn/completed`, `account/rateLimits/updated`) were
  written from documentation. If they are wrong, nothing works and there is no error
  message that says so. Add a startup capability probe that logs which methods
  succeeded and surfaces unsupported ones in the Control panel.

### 4.3 Hook bridge and hook installation

`daemon/internal/hooksbridge/hooksbridge.go`:

- **When the completion gate fires, the event is never forwarded.**
  `emitBlockDecision(); return 0` happens *before* `forward(payload)`. The daemon never
  learns the agent tried to stop, so the adapter's `stopped` flag and the timeline both
  miss it. Forward first (or concurrently), then emit the decision.
- **Double hook execution.** The Claude adapter passes its own hooks via `--settings`,
  *and* `hooks.InstallClaude()` writes the same hooks into `~/.claude/settings.json`.
  Claude Code merges both, so every event fires the bridge twice for managed runs — two
  block decisions, two forwarded payloads, duplicated timeline entries. Decide on one
  mechanism: global settings for *external awareness only* (no gate), `--settings` for
  the *managed gate*, and have the bridge deduplicate on `(session_id, event, turn)`.

`daemon/internal/hooks/hooks.go`:

- **`InstallCodex` rewrites the user's entire `~/.codex/config.toml`** from a
  `map[string]any` produced by `toml.Decode`, using a hand-rolled `encodeTOML`. This
  **destroys all comments and formatting**, and `encodeValue`'s `default` branch emits
  `fmt.Sprintf("%v", t)` — so a `time.Time` from a TOML datetime becomes an unquoted,
  syntactically invalid token. The function is documented as "NON-destructive". It is
  the opposite. Either (a) do a surgical text-level append/remove that preserves the
  original bytes, or (b) **do not touch `config.toml` at all** and find another
  awareness mechanism. Until fixed, make it refuse to run on any config it cannot
  round-trip byte-identically.
- Verify that Codex even *has* a `[hooks.*]` config surface in the installed version.
  If it does not, this whole path is writing junk into the user's config.
- `func changed(map[string]any) bool { return false }` is a stub that always returns
  false, called once for its side effect. Delete it and inline the intent.
- Backups accumulate forever (`path.<timestamp>.agent-conductor.bak`). Prune to the
  last 5.

### 4.4 Git worktrees: force-overwriting user branches

`daemon/internal/worktrees/worktrees.go`

```go
git(repo, "branch", "-f", branch, baseCommit)   // CreateIntegration
git(repo, "branch", "-f", branch, baseBranch)   // CardWorktree
```

`branch -f` **force-moves an existing branch**. Branch names come from
`batchNumber()`, which hashes the batch ID into `1..89` (§3.8) — so
`conductor/b12/integration` will be reused across batches, and `-f` will silently
destroy the previous batch's integration branch and any work on it. `Cleanup` then uses
`git branch -D` (force delete), destroying unmerged commits.

Fix:
- Use collision-free branch names derived from a real monotonic batch number plus a
  short random suffix.
- Create branches with `git branch <name> <commit>` (no `-f`) and **fail loudly** if
  the name is taken.
- `Cleanup` should use `git branch -d` and only fall back to `-D` when the branch is
  provably merged into the integration branch, or when the user explicitly confirms.

Also:
- `Precheck` checks `filepath.Join(repo, ".git", "MERGE_HEAD")` — wrong inside a
  worktree (where `.git` is a file) and wrong with a custom `--git-dir`. Use
  `git rev-parse --git-path MERGE_HEAD`.
- `Precheck` fails on **untracked files** (`git status --porcelain`). Untracked build
  output or `.env` files will block all parallel batches with a misleading "commit or
  stash your changes" message. Use `--untracked-files=no` for the blocking check and
  surface untracked files as a non-blocking warning.
- `git()` inherits `os.Environ()`, so a stray `GIT_DIR`/`GIT_WORK_TREE`/`GIT_INDEX_FILE`
  in the daemon's environment corrupts every operation. Explicitly unset them.
- `Apply` merges directly into the user's checked-out branch. Keep it, but take a
  safety ref first (`git branch conductor/backup/<ts>`) so the user can always get back.

---

## 5. THE HEADLINE FEATURE — Manual Pause with Continuation Timer

The owner's explicit requirement:

> *"for this queuing mechanism, I always want manual pause feature where I can pause the
> queuing and either keep it paused or set a continuation timer, that is continue after
> this much time."*

What exists today is nowhere near this: a single `Settings.Paused bool`
(`scheduler.go`), toggled by `PauseScheduler`/`ResumeScheduler`
(`commands.go:389`), whose only UI is a small button **buried inside a collapsed
"Runtime" accordion on the CONTROL tab** (`ControlView.tsx`). There is no timer, no
countdown, no scope control, no effect on running work, and no persistence semantics
beyond a boolean. Build the real thing.

### 5.1 Data model

Extend `Settings` (or better: introduce a dedicated `PauseState` persisted in its own
settings key so it is not clobbered by unrelated `updateSettings` round-trips — see
§5.5):

```go
type PauseState struct {
    Paused      bool       `json:"paused"`
    Scope       PauseScope `json:"scope"`        // QUEUE_ONLY | INCLUDE_ACTIVE
    Reason      string     `json:"reason"`       // user note, optional, shown in UI
    PausedAt    time.Time  `json:"pausedAt"`
    ResumeAt    *time.Time `json:"resumeAt"`     // nil = indefinite
    ResumeAfter string     `json:"resumeAfter"`  // original human input, e.g. "45m", for display
    Source      string     `json:"source"`       // user | quota | external-agent | schedule | api
    AutoResume  bool       `json:"autoResume"`
}
```

`PauseScope`:
- **`QUEUE_ONLY` (default).** No new run is dispatched. Runs already executing continue
  to completion. This is the safe, expected behaviour.
- **`INCLUDE_ACTIVE`.** Additionally sends a graceful interrupt to in-flight runs so
  they stop at a natural boundary, leaving them resumable in the *same native session*
  (`ContinueRun` path). This must never hard-kill a run mid-tool-use.

### 5.2 Daemon behaviour

- `schedule()` must **not** early-return wholesale when paused. Restructure so that
  while paused it still: resolves quota deadlines, reaps `CANCEL_REQUESTED` watchdogs,
  processes hook events, updates external-session awareness, and **evaluates the pause
  timer** — it only skips *dispatch*.
- A pause with `ResumeAt` set must be honoured **across daemon restarts**. Persist it;
  on startup, if `ResumeAt` is in the past, resume immediately and log it.
- The scheduler's wake calculation must include `ResumeAt` so the daemon wakes exactly
  at the deadline rather than up to a tick late.
- When the timer fires: clear the pause, write a `SCHEDULER_RESUMED` event, emit
  `pauseUpdated`, and `Poke()`.
- Idempotency: pausing while already paused **extends/replaces** the timer rather than
  erroring. Resuming while not paused is a no-op success.

### 5.3 RPC surface

Replace `pauseScheduler`/`resumeScheduler` with a richer, backward-compatible set:

| Method | Params | Behaviour |
|---|---|---|
| `pauseQueue` | `{ scope?, durationMs?, resumeAt?, reason? }` | Pause. `durationMs` and `resumeAt` are mutually exclusive; neither = indefinite. |
| `resumeQueue` | `{}` | Resume now, cancel any timer. |
| `extendPause` | `{ deltaMs }` | Add/subtract time from the current timer (supports "+15m" chips). |
| `clearPauseTimer` | `{}` | Keep paused, drop the deadline (→ indefinite). |
| `getPauseState` | `{}` | Full `PauseState`. Also embedded in every snapshot. |

Keep `pauseScheduler`/`resumeScheduler` as thin aliases so nothing breaks mid-migration.

Validation: reject `durationMs <= 0`; clamp to a sane maximum (7 days) with a clear
error; reject `resumeAt` in the past.

### 5.4 Automatic pauses (same machinery, honest labelling)

Reuse the same state with `Source != "user"` for system-initiated holds, so the UI has
one consistent place to explain "why is nothing running?":

- `Source: "quota"` — auto-pause when every enabled provider is quota-blocked, with
  `ResumeAt` = the max reset. Distinguish visually from a user pause.
- `Source: "external-agent"` — when `fifoBlockOnExternal` holds the queue.
- `Source: "schedule"` — see §7.4 (quiet hours).

A **user** pause always wins over a system pause and is never cleared automatically
except by its own timer.

### 5.5 Fix the settings-clobbering race first

Every settings mutation in the webview does read-modify-write:

```ts
const s = await rpc("getSettings", {});
await rpc("updateSettings", { ...s, someField: value });
```

(`ControlView.tsx` — `WeeklyOverride`, `AnchorSection.set`, `Staged.upd`.) Two concurrent
edits silently lose one. `Staged` additionally snapshots settings once on mount and
never re-syncs, so it writes back stale values over anything changed elsewhere.

Fix: add `patchSettings` taking a partial object, applied server-side under
`settingsMu`. Make the webview derive settings from the snapshot (single source of
truth), never from a separate fetch. This matters for pause because pause state must not
be lost to an unrelated settings write.

While there: `Staged` renders Codex model dropdowns from `s._codexModels`, a field that
**does not exist anywhere** — those selects are permanently empty. Source them from
`snapshot.providers.codex.models`.

### 5.6 UI for pause (see §6 for the visual language)

**Placement: top-level, always visible.** Pause is a primary control, not a settings
toggle. It belongs in the QUEUE header — not three clicks deep in CONTROL.

**Idle (not paused)** — a single quiet ghost button in the queue header:

```
  Agent Conductor                              ⏸  Pause
  Local · daemon 0.1.0 · Claude ● Codex ●
```

Clicking opens a small popover (not a modal — modals are heavy):

```
  ┌─────────────────────────────────────┐
  │  Pause queue                        │
  │                                     │
  │  ○ Until I resume                   │
  │  ● Resume automatically in…         │
  │                                     │
  │   [15m] [30m] [1h] [2h] [4h] [8h]   │
  │   ┌──────────────┐                  │
  │   │ custom  ⌄    │  or at  ──:──    │
  │   └──────────────┘                  │
  │                                     │
  │  ☐ Also pause the run in progress   │
  │     (stops at a safe point,         │
  │      resumable in the same session) │
  │                                     │
  │  Note (optional) ──────────────     │
  │                                     │
  │            [ Cancel ]  [ Pause ]    │
  └─────────────────────────────────────┘
```

**Paused** — the header becomes a calm, unmissable but non-alarming band. Amber, not
red. Pausing is a normal, intentional act, not an error:

```
  ┌──────────────────────────────────────────────────┐
  │  ⏸  Queue paused · resumes in 23:41              │
  │     3 runs waiting · "waiting on the API review" │
  │     [+15m]  [Clear timer]  [Resume now]          │
  └──────────────────────────────────────────────────┘
```

Requirements:
- **Live countdown**, `mm:ss` under an hour, `h:mm` above, tabular numerals, updating on
  a single shared 1s interval (one timer for the whole app, not one per component).
- **The countdown must be computed from the absolute `resumeAt` timestamp**, never by
  decrementing a local counter — otherwise it drifts and breaks when the machine sleeps.
- `+15m` / `+1h` chips call `extendPause`. `Clear timer` calls `clearPauseTimer`.
  `Resume now` calls `resumeQueue`.
- Status-bar item reflects it: `$(debug-pause) Paused · 23m`, with a tooltip showing the
  reason and exact resume time, clicking focuses the sidebar.
- Command palette: `Agent Conductor: Pause Queue…` (opens a QuickPick of the same
  durations), `Agent Conductor: Resume Queue`, `Agent Conductor: Extend Pause by 15 min`.
  Bind pause/resume to a suggested keybinding.
- When a system pause is active (`Source != "user"`), the band explains *why* in one
  plain sentence and offers the relevant escape hatch ("Claude quota resets at 3:15 PM —
  [Run Codex instead]").
- Announce state changes to screen readers via an `aria-live="polite"` region.
- **The queue list itself should show the consequence**: waiting rows dim slightly and
  the first one reads `starts when the queue resumes` instead of `next`.

### 5.7 Tests

Add scenario tests in `daemon/internal/scheduler/scheduler_test.go` using the existing
virtual-clock harness:

1. Pause indefinitely → queued runs stay `QUEUED`, active run completes normally.
2. Pause with a 1h timer → advance 59m: still paused. Advance 61m: dispatch resumes.
3. Pause `INCLUDE_ACTIVE` → active run receives a graceful interrupt and lands in a
   resumable state, not `FAILED`.
4. Pause with timer → simulate daemon restart → timer survives and still fires.
5. Pause with a timer already in the past on startup → resumes immediately.
6. `extendPause` moves the deadline; `clearPauseTimer` leaves it paused indefinitely.
7. A user pause is not cleared by a quota auto-resume.

---

## 6. Design mandate — premium, minimal, quiet

The owner's words:

> *"Ensure that the UI and user experience is premium minimalistic luxury feels, I do not
> like a cheap generic AI UI."*

The existing CSS (`webview/src/styles/global.css`) is a reasonable skeleton — VS Code
theme variables, tabular numerals, glyphs that are never colour-only, a single subtle
pulse animation, `prefers-reduced-motion` respected. **Keep that discipline.** But it
has never actually rendered (§1.2), it is thin, and the layout it dresses is generic.

### 6.1 Principles to hold to

1. **Typographic hierarchy, not decoration.** Depth comes from size, weight, letter-spacing
   and opacity — not from borders, shadows, gradients, or rounded "cards" everywhere.
   Luxury reads as restraint.
2. **One accent colour, used rarely.** Colour is information, never ornament. A screen
   where three things are coloured has three important things. Inherit the user's VS Code
   theme (`--vscode-*`) so the panel is part of their editor, not a foreign app. Never
   hardcode a brand palette.
3. **Generous, consistent negative space.** Commit to a 4px rhythm and never break it.
   The current `--ac-rhythm-*` tokens are right; apply them everywhere instead of the
   ~40 inline `style={{ marginTop: 8 }}` overrides scattered across the views (grep for
   `style={{` — almost every one is a rhythm violation and should become a utility class).
4. **Motion is meaning.** One pulse for "working". One 160ms ease for progress width.
   Nothing else moves. No spinners, no skeleton shimmer, no bouncing.
5. **Silence when idle.** An empty queue should look composed and deliberate, not like a
   broken screen. Real empty states with one clear next action.
6. **Numbers are typeset.** `font-variant-numeric: tabular-nums` on every duration,
   countdown, percentage, and count so nothing jitters as values change.
7. **Honesty over polish.** When quota percentages are unknown, say "unavailable" —
   never render a fake bar. The current code already does this; preserve it.

### 6.2 Concrete work

- **Density and rhythm.** Replace every inline `style` in `QueueView.tsx`,
  `ControlView.tsx`, `BatchesView.tsx`, `BatchPlanner.tsx`, `BatchDetail.tsx` with
  semantic utility classes driven by the rhythm tokens. Introduce `--ac-rhythm-4: 16px`
  and `--ac-rhythm-5: 24px` for section separation.
- **Type scale.** Define `--ac-text-xs/sm/base/lg` and one display size. Section titles
  are currently 11px uppercase with 0.08em tracking — good; make that a single reusable
  `.ac-eyebrow` and stop repeating the declaration.
- **The active run card is the hero.** Today it is a bordered box identical to everything
  else. It should be the visual anchor of the queue: larger title, live elapsed time,
  current stage (`Plan → Build`), the model, a hairline progress indication, and the two
  actions that matter (Open in Claude Code / Cancel). Everything else recedes.
- **The queue list is a list, not cards.** `.ac-qrow` is on the right track. Tighten the
  grid, right-align the numerals, and make the next-to-run row subtly distinguished (a
  2px accent rule on the leading edge, nothing more).
- **Composer.** The textarea is the primary input on the panel; give it room, a real
  placeholder, and a quiet affordance row beneath (`⌘⏎ to queue`). Provider/model/effort
  should collapse into a single compact line of controls that does not compete with the
  prompt. Persist the last-used provider/model/effort/permission per workspace so the
  user never re-picks.
- **Control tab.** Seven stacked accordions all collapsed by default is a filing
  cabinet, not a control surface. Restructure into: a short always-visible status
  summary (daemon, providers, usage bars), then progressive disclosure for the rest.
  Move Pause out of it entirely (§5.6).
- **Batch planner.** The row/card model is sound. Make it feel like arranging, not
  filling in a form: clear row numbering, obvious LINEAR vs PARALLEL affordance, drag
  handles in addition to the existing keyboard arrows, and a live one-line summary of
  what will happen ("3 rows · 5 runs · 2 parallel · ~clean tree required").
- **Accessibility is part of "premium".** Fix these real defects:
  - `BatchesView.tsx` renders `<button role="listitem">` inside `role="list"` — invalid
    ARIA. Use `<li>` wrapping a button, or `role="none"` + proper semantics.
  - `window.confirm` / `window.alert` are used in `QueueView.tsx`, `BatchPlanner.tsx`,
    and `BatchDetail.tsx`. **VS Code webviews are sandboxed and these do not work** — the
    Skip and Apply Batch confirmations either no-op or silently return. Replace with a
    proper in-webview confirmation affordance, or route through
    `vscode.window.showWarningMessage({modal:true})` on the host side.
  - Every interactive element needs a visible `:focus-visible` ring (the token exists;
    apply it consistently).
  - Tab order and arrow-key navigation in the tab strip is implemented but calls
    `.blur()` then `.focus()` — simplify with a roving `tabIndex`.
  - Verify contrast against both a light and a dark VS Code theme. `--ac-fg-muted`
    defaulting to `rgba(128,128,128,0.9)` is borderline on light backgrounds.
- **Respect the user's theme and their reduced-motion preference.** Already partly done;
  verify in High Contrast themes too, where `--vscode-editorWidget-border` behaves
  differently.

### 6.3 State store rewrite

`webview/src/state/store.ts` keeps module-level mutable state and forces re-renders with
`useState(0)` + a listener `Set`. It also stashes `detailBatchId` via `(state as any)`.
This is fragile under React 18 concurrent rendering and will tear.

Rewrite with `useSyncExternalStore` (built for exactly this), a properly typed store, and
selector subscriptions so a run update does not re-render the whole panel. Remove the
15-second blanket poll in favour of the incremental events from §2.5, keeping a slow
reconciliation tick as a safety net.

---

## 7. State of the art — additions you are explicitly authorised to make

The owner asked for *"the greatest comforting, simplest, highest level automation, user
experience."* Implement these in roughly this order. Do not add anything that requires
the user to understand a new concept before it helps them.

### 7.1 First-run experience that actually works
A guided panel on first activation: detect `claude` and `codex` (the unused
`detectProviderRuntimes()` already does this), check auth state, check whether the
background service is installed, check workspace trust. Each check shows ✓ / ✗ with a
**one-click fix button**, not a paragraph of instructions. Nothing about this product
should require reading the README to get started.

### 7.2 Durable notifications
When a run completes, fails, or needs attention while VS Code is closed, the user should
find out. Native OS notification on reconnect summarising what happened while they were
away ("2 runs completed, 1 needs attention"), plus an optional per-run "notify me"
toggle. Respect quiet hours (§7.4).

### 7.3 Queue templates and recall
Let the user save a composer configuration (provider/model/effort/permission/staged/
verification) as a named preset, and re-queue any past run with one click. Most real
usage is the same shape repeated.

### 7.4 Quiet hours / scheduled windows
A schedule that auto-pauses the queue outside chosen hours, built on the same `PauseState`
machinery (`Source: "schedule"`). Pairs naturally with the Window Anchor feature and is
the obvious completion of the pause work.

### 7.5 Drag-to-reorder the queue
`reorderQueue` exists in the RPC surface and has **no UI whatsoever**. Wire it up with
drag-and-drop plus keyboard alternatives (`Alt+↑/↓`), once §3.1's sort-key scheme is
coherent.

### 7.6 Cost and usage awareness
Track wall-clock active time and continuation counts per run (already persisted as
`ActiveNS`/`QuotaWaitNS`/`ContinuationCount`) and surface a weekly summary. Do not invent
dollar figures — show what is actually measured.

### 7.7 Run diffs at a glance
`worktrees.DiffStat` exists and is **never called**. Show "+142 −18 across 7 files" on
completed runs, with one click to open the diff.

### 7.8 Better daemon resilience
`recoverOnStartup` currently dumps every active run into `NEEDS_ATTENTION` after a daemon
restart. For Claude runs with a known `ProviderSessionID`, attempt an automatic
same-session `--resume` reconnect before giving up. That is the difference between
"survives restarts" as a marketing claim and as a fact.

### 7.9 Log rotation
`logging.New` opens one append-only file forever; the comment says rotation is "handled
by the service" and it is not. Add size-based rotation (10MB × 3) — and fix
`redactPatterns`, which uses `strings.Index` and therefore only redacts the **first**
occurrence of each pattern per line.

---

## 8. Security and privacy

1. **§3.3 — Delete must delete.** Highest priority item in this section.
2. **Prompt reveal warns, but the warning is weak.** `revealEffectivePrompt` detects
   secrets by substring-matching the literal string `"SHARED TESTING / SUDO BLOCK"`.
   Return an explicit `containsSecrets` flag computed from the run's `AttachSecrets`
   field instead of sniffing text.
3. **`ShellVerifier` runs `sh -c <user string>` with the full daemon environment.** That
   is the intended feature, but it should run with a sanitised environment (no
   `ANTHROPIC_*`, no vault paths), a configurable timeout, and output capped and redacted
   before persistence. `redactLines` currently only catches three keywords.
4. **Socket auth.** Constant-time token compare (§2.4); also verify the peer UID via
   `SO_PEERCRED` / `LOCAL_PEERCRED` where available.
5. **`TrustWorkspace` writes `~/.claude.json`.** It takes a backup, which is right. Also
   verify the path is inside a workspace the user actually opened, and never accept an
   arbitrary path from the webview without confirmation.
6. **Vault key.** `~/.agent-conductor/state/vault.key` at 0600 is the documented threat
   model and is fine. On macOS, offer optional Keychain storage as an upgrade.
7. **CSP.** Drop `'unsafe-inline'` for styles once §1.2 lands; add
   `img-src ${webview.cspSource} data:` explicitly rather than relying on `default-src 'none'`.

---

## 9. Build, packaging, operations

- **`scripts/build.mjs` copies the daemon to `dist/bin/agent-conductor`** while
  `configuration.ts` looks in `bin/` (§1.3). Make the path a single shared constant so
  they cannot drift again.
- **Ship all four platform binaries** (`darwin/arm64`, `darwin/amd64`, `linux/amd64`,
  `linux/arm64`) or build platform-specific VSIXs. Today the VSIX contains only the host
  build, so a VSIX built on an M-series Mac is useless on Linux — yet the README
  advertises Remote SSH to a Linux VPS as a headline use case.
- **`service.Install()`** — `os.Symlink(target, StablePath())` fails silently if the link
  already exists, so **upgrades keep pointing at the old binary**. Remove-then-link.
  Writing over a running binary also fails with `ETXTBSY` on Linux — write to a temp path
  and `rename`.
- **The launchd plist has no `EnvironmentVariables`.** LaunchAgents inherit a minimal
  `PATH` (`/usr/bin:/bin:/usr/sbin:/sbin`), so `findBinary("claude")` and
  `findBinary("codex")` will miss nvm, npm-global, asdf, and volta installs — which is
  most of them. Add an explicit `PATH` covering `~/.local/bin`, `/opt/homebrew/bin`,
  `/usr/local/bin`, `~/.npm-global/bin`, and the user's login shell `PATH` captured at
  install time. Add `StandardOutPath`/`StandardErrorPath` so crashes are diagnosable.
- **`lingerEnabled()`** runs `loginctl show-user "$USER"` with the literal string
  `$USER`, which always fails, silently falling through to a shell fallback. Use
  `os/user.Current().Username`.
- `package.json` has no `icon`, `repository`, or `bugs` field, and the version is
  hardcoded in three places (`package.json`, `main.go:Version`, `scripts/package.mjs`).
  Single-source it.
- `vitest.config.ts` sets `environment: "node"` globally while the webview suite needs
  jsdom; it currently works only because the npm scripts pass `--environment` explicitly
  and the test files carry `// @vitest-environment jsdom`. Use projects/workspaces so
  `npx vitest` does the right thing with no flags.
- Add CI: `go vet`, `go test -race`, `tsc --noEmit` ×2, `vitest`, and a VSIX smoke build.

---

## 10. Testing mandate

The current suite gives false confidence: 10 webview tests, 5 extension tests, and a Go
suite that exercises the scheduler only through the **fake** provider. Every P0 bug in
this document survived it.

Add at minimum:

**Regression tests for each P0** (these are the acceptance criteria):
1. Generated webview HTML contains `acquireVsCodeApi(` and a working style path.
2. `resolveDaemonBinary` finds the binary at the path `build.mjs` actually writes.
3. `go test -race ./...` passes.
4. `Codex.ensureStarted` completes against a stub stdio server (no deadlock) — with a
   test timeout so a regression fails fast instead of hanging CI.
5. `enqueueRun{runNext:true}` dispatches before earlier-queued runs.
6. Cancelling a `QUEUED` run reaches `CANCELLED` and frees its slot.
7. After `deleteSecretBlock`, a new run's effective prompt contains no secret section.
8. The full pause matrix from §5.7.
9. `newID` produces 10,000 unique IDs in a tight loop.
10. Worktree branch creation refuses to overwrite an existing branch.

**Integration:** extend `tests/integration/daemon-e2e.mjs` to drive the real socket
protocol end-to-end against `AC_MOCK=1`, including pause/resume with a timer.

**Manual acceptance, to be performed and reported:** build the VSIX, install it into a
real VS Code, open the sidebar, and confirm: styled UI renders, daemon connects, a mock
run queues and completes, pause with a 1-minute timer visibly counts down and resumes on
its own.

---

## 11. Suggested sequencing

| Phase | Content | Why first |
|---|---|---|
| **1** | §1 (extension DOA), §2.1–2.4 (crashes) | Nothing can be evaluated until the app runs and the daemon stops crashing |
| **2** | §3.1–3.5 (queue correctness), §3.3 (secret deletion) | Correctness and trust before features |
| **3** | §5 (pause + timer) incl. §5.5 settings-patch fix | The owner's explicit requirement |
| **4** | §4.2 (Codex deadlock), §4.4 (branch destruction) | Unblocks the second provider; stops data loss |
| **5** | §6 (design system) + §6.3 (store rewrite) | The experience mandate, on a now-stable base |
| **6** | §4.1 (Claude headless rewrite), §2.5 (performance) | Deep, high-value, needs room |
| **7** | §7 (automation), §8, §9, §10 | Elevation and hardening |

Ship §1–§3 as one reviewable commit series before starting §5 — the owner should be able
to use a working, correct version early rather than waiting for the whole programme.

---

## 12. What to report when you finish

- Every item above: **done / partially done / not done**, with a one-line reason for
  anything not done.
- Every place where reality (installed CLI behaviour, actual API surface) contradicted
  this brief, and what you did instead.
- The output of `go test -race ./...`, both typechecks, and the vitest suites.
- Confirmation that you built and installed the VSIX and that the manual acceptance
  checks in §10 pass — with honest notes on anything that did not.
- Anything you found that is not in this document. The audit was thorough but the
  codebase is ~14k lines and the provider integrations could not be verified against
  live CLIs.
