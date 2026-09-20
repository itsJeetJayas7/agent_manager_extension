# Remote VPS (Remote SSH) Operation

## Architecture

The extension declares `extensionKind: workspace`, so on Remote SSH the
extension host — and therefore the daemon, git, and provider runtimes — run
**on the VPS**. The sidebar header shows `Remote · <host>`.

Each VPS has its own:

- daemon + queue + SQLite state,
- encrypted vault,
- provider processes,
- worktrees.

There is no cross-VPS control plane in V1.

## Install on the VPS

1. Connect VS Code to the host (Remote SSH).
2. Install the Agent Conductor VSIX **on the remote** (`Extensions: Install
   from VSIX…` while connected — VS Code installs into the remote window).
3. Install provider CLIs on the VPS:
   - Claude Code: `npm install -g @anthropic-ai/claude-code` (official), then `claude` login.
   - Codex: official installer, then `codex login` once.
4. Command Palette → `Agent Conductor: Install Background Service`.

This installs a systemd **user** service:

```text
~/.local/share/agent-conductor/bin/<version>/agent-conductor
~/.local/bin/agent-conductor                                  (symlink)
~/.config/systemd/user/agent-conductor.service
```

with `Restart=always`, `RestartSec=3`, `WantedBy=default.target`.

## Surviving SSH logout: linger

systemd user services stop at session end unless lingering is enabled:

```bash
loginctl enable-linger $USER
```

If this needs admin rights, Agent Conductor **will not** run it for you —
run it yourself once, then verify:

```bash
loginctl show-user $USER | grep -i linger
```

Verify survival: queue a run, close the laptop / disconnect SSH, reconnect
after a while — the run continues, quota waits resume, and Window Anchor
fires from the VPS while you are away.

## Overnight behavior

- Quota-waiting runs resume at their persisted wall-clock deadline.
- Window Anchor (Codex; Claude experimental) fires from the daemon when the
  host is genuinely idle — VS Code closed, no queued work.
- After a daemon restart, at most ONE anchor is sent for the current window;
  missed windows are never "caught up".
