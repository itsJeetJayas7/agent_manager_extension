# Changelog

## 0.3.2 — live Claude Code view of managed runs

- **Open in Claude Code** now shows a running run live in Claude Code's own UI.
  Root causes: print-mode transcripts are tagged `sdk-cli`, which Claude Code
  leaves out of the conversations it can open (so it started a fresh one), and
  a Claude Code tab only follows a process it starts itself.
- Claude runs execute in one daemon-owned process per run using Claude's
  streaming input; continuations are written into the same process. Managed
  transcripts are tagged `agent-conductor`.
- Add `agent-conductor claude-wrapper`, installed as Claude Code's
  `claudeProcessWrapper` after you confirm: opening a managed session attaches
  the tab to the daemon's process; every other launch execs Claude unchanged.
  Stop, messages and mode/model changes in the tab reach that process. A
  session open in another Claude process is refused rather than forked.
- Pause the run's Claude process while Conductor opens a tab, so the history it
  loads and the live stream meet exactly.
- Report success only after the daemon sees the tab attached; run cards show
  attached tabs. Repeat clicks focus the same tab.
- Stop in Claude Code marks the run "Stopped from Claude Code"; Conductor's
  Cancel ends the process and its tool processes.
- New commands: Enable / Disable Live View in Claude Code. Uninstalling the
  service removes the setting; a missing wrapper script is removed on startup.
- The extension honors `AC_ROOT` like the daemon.

## 0.3.1 — queue order, controls and automatic workspace trust

- Trust all queued working folders in this personal installation, including
  prompt imports and generated worktrees. Remove the extra Claude trust gate
  and the extension's Restricted Mode block. Recover runs parked solely by the
  old trust gate on daemon startup while preserving explicit queue pauses.
- Keep new live arrivals at the end of the queue using persisted sort keys;
  reconnects and reorders now show the same saved order.
- Add visible first/up/down/last controls and a drop target at the end. Display
  rejected moves, support all waiting states, preserve sequence dependencies,
  persist moves atomically, and refresh the order in every connected window.


- Fix **Open in Claude Code** routing through the OS to the wrong IDE or
  workspace. Activate Claude and request the exact session in the current
  extension host; open a run's worktree in its own window with an expiring
  session handoff. Preserve Remote SSH authority and show actionable failures.
- Add **Live activity** to run cards: streamed Claude responses, tool events,
  last provider event age, checked process PID/liveness, and clear disconnected,
  quiet and unsupervised states. Stop animating progress from a saved RUNNING
  label alone. Recent output is bounded in daemon memory and redacted before
  display; native history remains the durable transcript. Restart/update the
  background service to enable activity monitoring.

- External Claude/Codex activity no longer pauses or blocks managed runs,
  including runs on the same provider, continuations and parallel batches.
  Remove the external-hold setting and clear old external holds on upgrade;
  explicit user pauses remain intact. External notification text no longer
  creates inferred quota holds; managed runs still handle real quota limits.
- Show an informational VS Code notification when an external turn starts
  while Conductor work is active, with **Show queue** and **Pause queue…**
  actions. Opening a session, tool events and reconnecting do not repeat the
  alert. Notifications can be disabled with
  `agentConductor.notifyOnExternalOverlap` without changing scheduling.
- Preserve external activity through compaction and detect successive turns
  even when they start less than ten seconds apart. Queue and Control report
  external activity consistently as information, not a scheduling hold.
- Fix Claude turns failing on older VPS runtimes with `unknown option
  '--permission-prompts'`. Probe the installed CLI before passing this optional
  flag, preserving the selected permission mode for new and resumed turns.
- Fix the sidebar crashing when Control opens with no runs in the weekly
  usage window. Return an empty recent-runs array from the daemon and handle
  null or missing history from older daemons in the panel.

## 0.3.0 — prompt sequences from folders

- Import a folder from the queue composer or **Agent Conductor: Import
  Prompts from Folder** in the Command Palette. Preview each prompt as plain
  text, choose the working folder separately, and reuse composer settings.
- Read numbered UTF-8 Markdown files (`001-short-title.md`) in numeric order.
  Report gaps, duplicate numbers, invalid numbered filenames, empty or invalid
  text, symbolic links and size limits before anything is queued. Other files
  and unnumbered subfolders are listed as ignored.
- Queue the entire sequence atomically as LINEAR batch rows. A failed,
  cancelled or blocked step holds later steps until resolved or explicitly
  skipped. Existing pause, quota, continuation and restart recovery apply.
- Detect prompt changes between preview and import. Retrying the same import
  after a lost response returns the original batch, including after a daemon
  restart. Queued prompts are encrypted snapshots, independent of later edits.
- Include a copyable generator prompt and remember the last prompt folder per
  workspace. Folder selection and reading run on the workspace host for
  Remote SSH support. Update/restart the background service with this release
  to enable the new import endpoint.

## 0.2.0 — overnight recovery and explicit model controls

- Pin Claude Opus 5.5 by its full model ID; show named, versioned models and
  all supported effort levels, including medium and xhigh. Mark moving
  aliases, clear incompatible efforts and expose complete staged presets.
  Run cards report the actual provider model and effective stage effort.
- Queue every continuation through the scheduler, preserving pause, quota,
  external-session, row and concurrency limits. Persist retry deadlines,
  encrypted continuation prompts, verification/audit counts and session IDs.
- Recover transient network, overload and process failures with bounded
  exponential retries; continue missing completion/plan markers up to a
  configurable limit. Keep explicit blockers and permanent failures visible.
- Fix premature slot release, stale completed-batch writes, audit loops,
  cancellation during verification, pause/resume races and restart recovery.
- Reap provider processes and tool subprocesses on timeout/cancellation,
  including the legacy PTY path; bound verification output in memory.
- Treat quota warnings as warnings, preserve external busy status through
  tool notifications, and honor weekly reset overrides after their deadlines.
- Collapse concurrent capability refreshes; test 100 queued runs and 500
  snapshot requests. Add real Chrome layout/interaction checks and live
  Opus 5.5 session continuation coverage.
- Fix Linux service upgrade/error handling, quote service paths correctly,
  and install provider hooks through the stable daemon launcher.
  Package macOS and Linux binaries for both x64 and ARM64.
- Upgrade build/test tooling to resolve the dependency audit findings and
  repair the local reinstall script. Restrict the VSIX to runtime artifacts
  and public metadata so local agent worktrees are not shipped. See `docs/overnight-validation.md` for
  measured results and the limits of this validation.

## 0.1.2 — verified against the real Codex and Claude binaries

Everything below was found by running both real CLIs through the real daemon
(`npm run test:live`), not by reading code.

### Fixed — Codex reported "runtime missing" while installed and signed in

- **Discovery.** Both CLIs are commonly installed *only* inside their official
  VS Code extension (`openai.chatgpt/bin/<triple>/codex`,
  `anthropic.claude-code/resources/native-binary/claude`) and never land on
  `PATH`. Discovery now searches those bundles — VS Code, Insiders, the remote
  server, Cursor, Windsurf, VSCodium — after `PATH` and the usual install
  locations, preferring the newest copy. `AC_CODEX_BIN` / `AC_CLAUDE_BIN`
  override it.
- `agent-conductor diagnostics` called `exec.LookPath` directly and so could
  disagree with the sidebar. Both now answer from the same lookup, and
  diagnostics prints the resolved path when it is not the one on `PATH`.
- Codex only reported `installed` *after* the app-server handshake succeeded,
  so a handshake failure was shown to the user as a missing runtime. The
  binary's presence and the app-server's health are now reported separately.

### Fixed — three Codex protocol payloads were wrong

Verified against the installed binary's own schema
(`codex app-server generate-json-schema`):

- `thread/start` takes `sandbox` as a kebab-case `SandboxMode`
  (`danger-full-access`), while `turn/start` takes `sandboxPolicy` as a tagged
  object (`{"type":"dangerFullAccess"}`). The adapter sent the camelCase tag to
  both, so **every real Codex run failed at `thread/start`**.
- `model/list` returns `{data: [...]}`, not `{models: [...]}` — the model
  picker was always empty. Effort ladders now come from each model's
  `supportedReasoningEfforts`.
- `account/read` returns `{account: {type, planType, email}}`, not a flat
  `{authMode, plan}` — auth always displayed as "unknown" for a signed-in
  account.

### Fixed — the Usage panel could only ever show zeros

`ActiveNS`, `QueueWaitNS` and `QuotaWaitNS` were persisted, summed by
`UsageSince` and rendered in the UI, but nothing ever incremented them: the
only assignment in the codebase was `x.ActiveNS = r.ActiveNS`. Dwell time is
now accounted in `TransitionRun`, the single funnel every state change passes
through, on a clock the scheduler injects. A requeued run no longer charges its
original wait twice.

### Fixed — a spurious refused transition on every queued run

`markWaitingExternal` decided from the in-memory run slice that `dispatchRow`
had already invalidated in the same tick, so the moment a waiting run got its
slot the scheduler asked the store to send it back to `QUEUED`. The transition
table refused it — no run was ever corrupted — but each refusal landed in the
daemon log and on the user's run timeline as
`refused: illegal transition STARTING -> QUEUED`.

### Added

- `npm run test:live`: queues one real Claude run and one real Codex run
  against the same file in a throwaway git repo, asserts the two never
  executed concurrently, and verifies each agent's signed line is present and
  correctly numbered. A lost update or a concurrent dispatch fails the test.
- Unit tests pinning the Codex enum spellings and the bundled-CLI lookup, so
  these regressions fail in CI rather than on a user's first real run.

## 0.1.1 — remediation

The 0.1.0 VSIX did not work. This release fixes that, then goes considerably
further. Full detail is in the commit series; the headlines:

### Fixed — the extension was dead on arrival

- `acquireVscodeApi` was a typo for `acquireVsCodeApi`. It threw before React
  mounted, so the sidebar was an empty shell that never talked to anything.
- The stylesheet was built to `dist/webview/assets/main.css` and never
  referenced, so the entire design system was inert. CSS is now folded into
  the bundle: one artifact, no flash of unstyled content.
- The daemon binary was looked up in `bin/` while the build wrote `dist/bin/`,
  so it was never found and the UI said "offline" forever. Both sides now read
  one shared constant. Resolution also restores the execute bit that VSIX
  extraction drops, validates the binary, and reports quarantine or a wrong
  architecture with the exact command that fixes it.

### Fixed — the daemon crashed, deadlocked and leaked

- `quota.Guardian` had no mutex. Concurrent map access is a *fatal*,
  unrecoverable Go runtime error, not a warning.
- The Window Anchor closed a channel from the receiving side while the
  producer was still live: "send on closed channel" killed the daemon.
- The Codex adapter deadlocked unconditionally on first use and never released
  its lock, so every later Codex operation blocked forever.
- The API server closed a client channel out from under a live sender, leaked
  a goroutine and a 256-slot channel per reconnect, and never unsubscribed.
- `go test -race ./...` passes; it failed before.

### Fixed — the queue was wrong

- **"Run Next" put the run last.** Three separate defects: an inverted sort
  key, `Store.UpdateRun` never persisting `sort_key` at all, and the scheduler
  walking batches in newest-first order so run order was irrelevant.
- Cancelling a queued run parked it in `CANCEL_REQUESTED` forever and
  permanently consumed a slot. Two cancels killed the queue.
- **"Delete Shared Testing & Sudo Block" deleted nothing.** It set an unread
  flag; the plaintext stayed in the database, was still revealable, and was
  still shipped to the provider on the next run.
- `newID()` collided within a clock tick, aborting batch creation half-way.
- Git branches were created with `branch -f` under colliding names, silently
  destroying previous batches' integration branches and their commits.
- `InstallCodex`, documented as non-destructive, rewrote the user's entire
  `~/.codex/config.toml`, destroying comments and formatting and emitting
  invalid TOML for datetimes.

### Fixed — found by installing the VSIX into a real VS Code

- The daemon had **no single-instance guard**. A second one removed the socket
  file and listened in the first one's place, so the first became unreachable
  while its scheduler kept running: two schedulers on one database, and the
  older one started runs while the queue was paused. There is now an advisory
  lock per state directory.
- The extension re-spawned a daemon every 1.5 seconds, forever, whenever
  connecting failed for a reason starting a daemon cannot fix. Spawns now
  follow the reconnect backoff, and after five attempts it says so.
- The extension only activated when the sidebar was opened, so the status bar
  item, the reconnect loop and the away-summary notification never appeared on
  their own. It activates on startup now.
- It now declares that it does not support untrusted workspaces, so VS Code's
  Restricted Mode explains itself instead of silently doing nothing.

### Added — manual pause with a continuation timer

Hold the queue indefinitely or until a deadline. Live countdown from an
absolute timestamp (survives sleep), survives daemon restarts, `+15m`/`+1h`,
clear-timer, resume-now, an optional note, and an option to stop the run in
progress at a safe point and continue it in the same session afterwards.
Quota, external agents and quiet hours reuse the same state, so there is one
honest answer to "why is nothing running?".

### Added

- Claude Code driven through its headless `stream-json` protocol instead of
  puppeteering the interactive TUI through a PTY. Structured quota from
  `rate_limit_event` at `exact` confidence.
- Quiet hours; drag-to-reorder; a first-run checklist where every failing
  check carries its own fix; away-summary notifications; usage measured
  honestly; run diff stats; restart reattachment to the same native session.
- A rebuilt design system, and a webview store on `useSyncExternalStore` fed
  by incremental events rather than a full snapshot per event.
- CI: `go vet`, `gofmt`, `go test -race`, four-target cross-compilation, both
  typechecks, vitest, a VSIX smoke build, an end-to-end protocol test and a
  test that boots the real bundle in the real host HTML.

## 0.1.0

Initial release.
