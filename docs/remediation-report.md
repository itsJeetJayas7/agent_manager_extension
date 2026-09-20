# Remediation report

Status of every item in `AGENT_BRIEF.md`, honestly. Written for the repository
owner and whoever reviews the commit series.

**Verification gates at the time of writing** — all green:

| Gate | Result |
| --- | --- |
| `go vet ./...` | clean |
| `gofmt -l ./internal ./cmd` | clean |
| `go test -race -count=1 ./...` | 10 packages ok, 0 failures (it failed before: `TestWeeklyExhaustionSuppressesAnchor`) |
| `tsc -p tsconfig.json --noEmit` | clean |
| `tsc -p webview/tsconfig.json --noEmit` | clean |
| `vitest run` | 9 files, 77 tests, all passing (was 15 tests) |
| `tests/integration/daemon-e2e.mjs` | 12/12 sections pass over the real socket protocol |
| `tests/integration/webview-boot.mjs` | real bundle boots in the real host HTML |
| VSIX | 19.1MB, 4 platform daemons, installs and runs in VS Code 1.138.0 |

## §1 — extension dead on arrival

| Item | Status |
| --- | --- |
| 1.1 `acquireVscodeApi` typo + idempotent guard | done |
| 1.2 stylesheet never loaded → inlined into the bundle (option B) | done |
| 1.3 daemon binary path, chmod, validation, quarantine detection | done |
| 1.4 all ten host/client defects in the table | done |

`style-src 'unsafe-inline'` is **kept**, deliberately. CSP3 ignores
`'unsafe-inline'` whenever a nonce or hash is also present, and VS Code injects
its own `_defaultStyles` `<style>` into every webview document. Adding a nonce
would break VS Code's own theming. The design system itself ships inside
`webview.js`, so there is no external stylesheet to permit, and `img-src` is
now explicit rather than inherited from `default-src`.

## §2 — daemon crashes

| Item | Status |
| --- | --- |
| 2.1 `quota.Guardian` mutex + defensive copy + dead `any` | done |
| 2.2 `Conductor.clock` atomic | done |
| 2.3 anchor send-on-closed-channel, claim ordering, three case bugs | done |
| 2.4 API close/leak/idempotent-subscribe/timeout/constant-time/dead type | done |
| 2.5 all five performance items | done |

## §3 — scheduler and queue

| Item | Status |
| --- | --- |
| 3.1 sparse sort keys, Run Next, reorder | done — see note |
| 3.2 cancel a queued run + 60s watchdog | done |
| 3.3 secret deletion really deletes | done |
| 3.4 `newID` collision-free + `ApplyBatch` in one transaction | done |
| 3.5 `ContinueRun` stale state; audit of ~26 discarded transitions | done |
| 3.6 `UpdateRun` as a true patch | done |
| 3.7 `CopyBatchReport` empty provider | done |
| 3.8 every row of the table | done, except one partial |

**Note on 3.1.** The inverted sort key was only one of three reasons Run Next
did not work. `Store.UpdateRun` never wrote `sort_key` at all, so every reorder
was a silent no-op; and the scheduler walked batches in `ListBatches` order
(newest-first, for the UI), so in the normal queue — where each run is its own
implicit batch — run sort keys were irrelevant to dispatch order. All three are
fixed. A fourth, found later by the e2e test: the snapshot returned runs
grouped by batch, so the sidebar's "Up next" disagreed with dispatch order.

**Partial:** `beginVerification`'s timeout is now a setting, but progress is
reported as a single start event rather than streamed output. Streaming a live
command's stdout would need a new event channel through the protocol; the
timeout was the part that trapped people.

## §4 — provider adapters

| Item | Status |
| --- | --- |
| 4.1 Claude headless rewrite + every listed bug | done |
| 4.2 Codex deadlock, no-op Cancel, zombie, restart, `parseEfforts`, `codexEnv` | done |
| 4.2 verify Codex JSON-RPC method names against the installed binary | **not done — `codex` is not installed on this machine** |
| 4.3 hook bridge forward-before-gate, double execution, `InstallCodex`, `changed()`, backups | done |
| 4.3 verify Codex has a `[hooks.*]` surface | **not done — same reason** |
| 4.4 branch destruction, `Precheck`, git env, safety ref | done |

Mitigations for what could not be verified are in
`docs/provider-capabilities.md`: the adapter records the outcome of every
JSON-RPC method it calls, and `InstallCodex` appends one delimited, removable
block instead of rewriting the user's config.

## §5 — pause with a continuation timer

Every sub-item done, including all seven scenario tests in §5.7 and the
§5.5 settings-clobbering fix (`patchSettings`) and the `_codexModels`
phantom field.

One deliberate design decision: an `external-agent` hold is *recorded* in
`PauseState` so the UI has one place to explain itself, but it does not gate
dispatch, because `canStart` already enforces it more precisely (a PARALLEL row
may proceed while a manual session is busy). `PauseState.GatesDispatch()` makes
that explicit.

## §6 — design

| Item | Status |
| --- | --- |
| 6.1 principles | held to |
| 6.2 rhythm, type scale, hero, list, composer, control tab, planner | done |
| 6.2 accessibility defects (ARIA, sandboxed dialogs, focus, roving tabindex) | done |
| 6.2 verify contrast in light, dark and High Contrast themes | **partial — not visually verified** |
| 6.3 store rewrite on `useSyncExternalStore` | done |

Contrast: the muted-foreground fallback was darkened (the old
`rgba(128,128,128,0.9)` was borderline on light backgrounds), everything else
derives from `--vscode-*` so it follows the user's theme, and a
`forced-colors` block was added for High Contrast. It has **not** been
visually inspected in each theme, because screen capture is not permitted for
this environment. That check is still worth doing by eye.

## §7 — additions

| Item | Status |
| --- | --- |
| 7.1 first-run experience | done |
| 7.2 durable notifications | done (away summary, quiet-hours aware) |
| 7.2 per-run "notify me" toggle | **not done** — would need a new run column and UI for a narrow case; the away summary covers the stated need |
| 7.3 templates and recall | done |
| 7.4 quiet hours | done |
| 7.5 drag-to-reorder | done |
| 7.6 cost and usage awareness | done |
| 7.7 run diffs at a glance | done |
| 7.8 daemon resilience | done |
| 7.9 log rotation + `redactPatterns` | done |

## §8 — security and privacy

| Item | Status |
| --- | --- |
| 8.1 delete must delete | done |
| 8.2 `containsSecrets` from the run, not from text | done |
| 8.3 `ShellVerifier` sanitised env, timeout, capped + redacted output | done |
| 8.4 constant-time token + peer UID | done |
| 8.5 `TrustWorkspace` path validation + confirmation | done |
| 8.6 vault key at 0600 | unchanged (documented threat model) |
| 8.6 optional macOS Keychain storage | **not done** — explicitly an optional upgrade; it changes the recovery story and deserves its own decision |
| 8.7 CSP `img-src` explicit | done |
| 8.7 drop `style-src 'unsafe-inline'` | **not done, deliberately** — see §1 above |

## §9 — build, packaging, operations

All done: shared path constant, four platform binaries with a packaging guard,
remove-then-link and write-then-rename in `service.Install()`, explicit `PATH`
captured from the login shell in both unit templates, launchd log paths,
`lingerEnabled` fixed, `package.json` metadata, single-sourced version, vitest
workspaces, and CI.

## §10 — testing

All ten regression tests exist, plus the extended e2e and the webview boot
test. Manual acceptance was performed against the installed VSIX in VS Code
1.138.0 — see the next section.

## Manual acceptance — what was and was not confirmed

**Confirmed, against the VSIX installed into a real VS Code:**

- the extension activates and starts the daemon by itself;
- exactly one daemon runs;
- providers probe correctly (`claude 2.1.278` detected, `codex` absent);
- a mock run completes and records its native session;
- a 60-second pause counts down (`01:00 → 00:39 → 00:19 → fired`), holds a
  newly queued run at `QUEUED` throughout, fires on its own, and the held run
  then completes;
- `runUpdated` / `batchUpdated` / `pauseUpdated` reach a live subscriber;
- the daemon binary keeps its execute bit through real VSIX extraction and
  carries no quarantine attribute on this machine.

**Not confirmed:** the sidebar was not *looked at*. Screen recording is not
permitted for this environment, so no screenshot could be taken. The closest
available substitute is `tests/integration/webview-boot.mjs`, which boots the
real built bundle inside the real generated host HTML and asserts that React
mounts, 16.5KB of design system is injected, the tab strip renders and the
webview posts `ready` and `requestSnapshot`. **Someone should still open it and
look at it.**

## Found here, not in the brief

1. **The daemon had no single-instance guard.** `Listen()` removes the socket
   file and listens in its place, so a second daemon silently took over while
   the first one's scheduler kept running — two schedulers against one SQLite
   database. The older one, never told about the pause, started runs while the
   queue was paused. Fixed with an advisory lock per state directory. This is
   the most serious thing found that the brief did not list.
2. **The extension re-spawned a daemon every 1.5 seconds, forever**, whenever
   connecting failed for a reason starting a daemon cannot fix. That is what
   produced the pile of daemons above. Fixed with backoff and a warning.
3. **`activationEvents: []`** meant the extension only woke when the sidebar
   was revealed, so the status bar, the reconnect loop and the away-summary
   notification never appeared on their own — which defeats §7.2 entirely.
4. **`Store.UpdateRun` never persisted** `sort_key`, `provider`, `model`,
   `effort`, `permission`, `staged` or `attach_secrets`, so queue edits and
   every reorder were silent no-ops regardless of the sort-key scheme.
5. **The scheduler dispatched batches newest-first**, because it reused the
   UI's `ListBatches`. Its test only passed because identical `created_at`
   values tied and fell back to insertion order.
6. **`pickActiveState` returned `AUTO_RESUMING`**, a state with no legal edge
   to `COMPLETED`, so any resumed run would have wedged. It only ever worked
   because callers passed a stale pre-transition copy of the run.
7. **`ORPHANED` was unreachable** from most dispatched states, and
   `continueRun` asked for a transition it had not checked was legal and gave
   up silently when refused.
8. **Claude Code does expose structured quota** — see
   `docs/provider-capabilities.md`. Both the code and this repo's own docs said
   otherwise.
9. **VS Code Restricted Mode** silently disabled the extension with no
   explanation; it now declares that it needs a trusted workspace.
