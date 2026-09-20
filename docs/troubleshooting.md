# Troubleshooting

## Daemon offline badge in the sidebar

- `Agent Conductor: Start Development Daemon` runs `agent-conductor serve` in
  a terminal (dev mode).
- `Agent Conductor: Install Background Service` installs the persistent
  service (systemd user unit on Linux, LaunchAgent on macOS).
- Check the socket exists: `ls -l ~/.agent-conductor/run/conductor.sock`.
- Logs: `~/.agent-conductor/logs/daemon.jsonl` (redacted).

## Runs never start

1. Is the queue paused? Control → Runtime → Resume Queue.
2. Is an external agent busy? The Queue tab shows
   `Waiting — Claude Code is active outside Agent Conductor`. Manual sessions
   are never killed; either wait or use Start-anywhere behavior by queueing
   into a parallel batch (same-provider blocking only).
3. Provider runtime missing? Run `Agent Conductor: Diagnostics`.

## Claude runs bill to an API key unexpectedly

`ANTHROPIC_API_KEY` in the daemon's environment is stripped from managed
children by default (subscription protection). If you see API billing,
verify nobody enabled API mode in Control → Providers, and check the
warning banner shown when the key is detected.

## External session stuck "busy"

The Control tab lists external sessions with a conservative 45-minute
staleness policy. If a session is genuinely stale (crashed editor), use
`Clear stale busy state` next to it.

## Parallel batch blocked: "clean Git worktree required"

Parallel rows refuse to run over dirty trees. Agent Conductor will never
stash, commit, or discard your changes — commit or stash manually, then
queue the batch again.

## Integration conflict

On a merge conflict the batch stops in `AWAITING_INTEGRATION` with every
worktree preserved. Resolve manually in the integration worktree
(`~/.agent-conductor/worktrees/<repo>/<batch>/integration`), or inspect card
worktrees, then decide: retry integration, skip the conflicting run, or
cancel the batch.

## Hook installation refused

If `~/.claude/settings.json` or `~/.codex/config.toml` is unreadable
(invalid JSON/TOML), Agent Conductor refuses to touch it. Fix the file by
hand (a timestamped `.agent-conductor.bak` exists from earlier installs),
then run `Agent Conductor: Repair Runtime`.

## Codex thread not visible in the extension

Managed Codex threads live in the same `CODEX_HOME` history as the IDE
extension. If the exact thread does not appear immediately, reload the Codex
view; the deterministic `[AC Bxx-Ryy-Czz] title` makes it findable. Exact
programmatic thread focusing depends on documented public commands in your
Codex extension version.

## Uninstall

`Agent Conductor: Uninstall Background Service & Hooks` removes the service
and only Agent Conductor's provider hook entries. State (runs, encrypted
secrets) remains in `~/.agent-conductor` — delete that directory to erase.
