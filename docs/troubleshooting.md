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
2. Check provider quota, managed concurrency limits, configured quiet hours
   and the preceding batch row. External agents never hold the queue, even
   on the same provider. Old external holds clear when the updated daemon starts.
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
This status is informational and does not block managed runs.

## Running an agent alongside Conductor

External Claude/Codex work can overlap FIFO runs, continuations and parallel
batches. When an external turn starts while Conductor work is active, VS Code
shows a notification. **Show queue** opens the sidebar; **Pause queue…** opens
the manual pause control. Dismissing the notification leaves everything running.
Opening a session or receiving tool events does not repeat the notification.
Set `agentConductor.notifyOnExternalOverlap` to `false` to disable these alerts.

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

## Open in Claude Code shows no conversation, or does not update

Two causes, now both addressed (details in
[claude-native-live-view.md](claude-native-live-view.md)):

- Claude Code hides sessions created by `claude -p` (tagged `sdk-cli`), and
  opening a session it cannot find starts a fresh conversation. Managed runs
  are now tagged `agent-conductor`. Runs started by an older daemon remain
  hidden from Claude Code's history.
- A Claude Code tab only follows a process it is attached to. Live view routes
  Claude Code's process launch through Agent Conductor's wrapper so the tab
  attaches to the daemon-owned run.

If Conductor reports that Claude Code "did not attach":

1. Check `claudeCode.claudeProcessWrapper` in your user (or Remote) settings
   points to `~/.agent-conductor/bin/agent-conductor-claude-wrapper`; run
   **Agent Conductor: Enable Live View in Claude Code** to set it.
2. Make sure the daemon is current: an older daemon cannot relay. Run
   **Agent Conductor: Install Background Service** and reload.
3. Read `~/.agent-conductor/logs/claude-wrapper.log`: each launch records
   `attached`, `passthrough (reason)` or `refused (held by pid N)`. A refusal
   means another Claude process (for example a terminal) has the conversation
   open; close it there.

Run status comes from the daemon, not from the tab: the run card shows
"Live in Claude Code · N tabs attached" only while the daemon relays to a tab.

If activity is unavailable after upgrading, run **Agent Conductor: Install
Background Service** again to replace/restart the service binary, then reload
the VS Code window. **Repair Runtime** repairs hooks only. Recent activity is bounded in daemon memory and is reset by a daemon
restart or new turn. Use the native session for the complete saved transcript.
