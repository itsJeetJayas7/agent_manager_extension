# Changelog

## 0.1.0 — 2026-09-19

Initial release.

- VS Code sidebar (QUEUE / BATCHES / CONTROL) + Batch Planner + Batch Detail panels.
- Persistent Go daemon: SQLite WAL state, validated run state machine, FIFO
  scheduler with per-provider slots and external-agent awareness, batch rows
  (LINEAR/PARALLEL) with barriers and Git worktree isolation + guarded apply.
- Claude adapter: interactive PTY runtime, `--session-id`/`--resume`,
  subscription protection (ANTHROPIC_API_KEY stripped), lifecycle-hook state,
  completion gate via managed Stop hook (single continuation, no loops).
- Codex adapter: `codex app-server` JSON-RPC; dynamic model discovery,
  account state, thread lifecycle, structured rate limits, interrupt.
- Non-destructive, namespaced awareness hooks for both providers with
  backups, idempotent install, and surgical uninstall.
- Quota guardian: five-hour + weekly windows, max-of-exhausted resume times,
  same-session auto-continue after resets, weekly-reset override for Claude.
- Window Anchor with real-work priority and exactly-once claims
  (Codex structured; Claude experimental).
- Staged Intelligence (PLAN → BUILD, optional AUDIT) on the same native
  conversation for both providers.
- Optional post-run verification commands with bounded agent fix loops.
- AES-256-GCM vault for prompts and the Shared Testing & Sudo Block; JSON
  logs with pre-write redaction; strict-CSP webviews.
- systemd user service / LaunchAgent lifecycle with lingering guidance.
- Test suites: Go unit + scheduler scenarios (virtual clock), vitest for
  extension/webview.
