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
3. Install provider CLIs on the VPS (Opus 5.5 requires Claude Code 2.1.280+;
   Sonnet 5.5 requires 2.1.284+; this release was tested with 2.1.288):
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

## Claude exits with `unknown option '--permission-prompts'`

The daemon was passing a flag added in Claude Code 2.1.259 to an older CLI.
The compatibility fix checks the installed CLI's `--help` before adding the
flag and preserves your selected permission mode. Install the rebuilt VSIX
on the remote host, then run **Agent Conductor: Install Background Service**
again to replace and restart the persistent daemon before retrying the run.
Reloading VS Code alone does not replace the running service.

To check which runtime the daemon finds, run
`~/.local/bin/agent-conductor diagnostics` on the VPS. Check that binary with
`--version` and `--help`; `AC_CLAUDE_BIN` can override the one on your shell's
PATH. Updating that CLI also resolves the unsupported flag, and newer models
still require the versions listed above.

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

## Release 0.2.0 checks

Use the explicit `claude-opus-5-5` model and select low, medium, high, xhigh
or max. Staged Intelligence has independent model/effort settings; its BUILD
default is Sonnet 5.5, so change it if every stage should use Opus 5.5.
Run cards show the model reported by the provider.

Check the service before disconnecting:

```bash
systemctl --user is-active agent-conductor.service
~/.local/bin/agent-conductor diagnostics
journalctl --user -u agent-conductor.service --since '15 minutes ago'
```

The installer now restarts the service during upgrades and surfaces failed
systemctl commands. Hooks use the stable launcher so removing an old VS Code
extension version does not break them.

The [overnight validation report](overnight-validation.md) contains measured
load results, the recovery behavior for each failure class and the remaining
VPS smoke test. Linux binaries were cross-compiled on macOS; they still need
that first runtime check on your actual VPS.
