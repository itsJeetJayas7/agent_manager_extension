# Changelog

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
