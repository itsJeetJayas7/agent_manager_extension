# Agent Conductor

Queue, supervise, and orchestrate **Claude Code** and **Codex** coding-agent
runs from one quiet VS Code sidebar — then let them run unattended. A
persistent daemon owns execution: closing VS Code, sleeping your laptop, or
dropping the SSH connection does **not** kill managed work. Every managed run
keeps its native provider session/thread so you can always inspect it in the
official Claude Code / Codex extensions.

```text
VS Code sidebar ── Unix socket ── Conductor daemon ── Claude (PTY) / Codex (app-server)
                                       │
                              SQLite (WAL) + AES-256-GCM vault + quota guardian
```

## What it does

- **Queue composer**: paste a prompt verbatim (no AI rewriting), pick
  provider / model / effort / permission (Bypass remembered), attach the
  encrypted **Shared Testing & Sudo Block**, enable **Staged Intelligence**.
- **FIFO queue with awareness**: one managed run at a time by default;
  manually-started Claude/Codex sessions block the queue (never killed).
- **Batches**: LINEAR and PARALLEL rows; parallel cards run concurrently in
  isolated **Git worktrees**, integrate into an internal branch, and barriers
  hold the next row until every card succeeds. Failures block subsequent rows
  until retried or explicitly skipped.
- **Completion contract**: managed runs must end with
  `<AC_RESULT>COMPLETE</AC_RESULT>` or `<AC_RESULT>BLOCKED: reason</AC_RESULT>`.
  A stop hook blocks the first marker-less stop once; the second lands in
  `NEEDS_ATTENTION` — no infinite loops, external sessions never gated.
- **Quota guardian**: five-hour and weekly windows normalized per provider;
  Codex from structured `account/rateLimits` (exact), Claude from lifecycle
  events (medium confidence, honestly labeled). Runs wait, then resume the
  **same native conversation** after the max of all exhausted resets.
- **Window Anchor**: while genuinely idle, one tiny request per quota window
  keeps the clock running overnight on a VPS. Real queued work always wins;
  exactly-once per window; weekly exhaustion suppresses five-hour anchors.
  Claude anchoring is labeled *Experimental*.
- **Staged Intelligence**: PLAN on a strong model (read-only), BUILD on a
  cheaper model in the **same native conversation**, optional single AUDIT
  cycle.
- **Verification hooks**: optional commands (e.g. `npm test`) run in the
  run's worktree after COMPLETE; failures bounce back to the same agent
  session once or twice, then `NEEDS_ATTENTION`.
- **Deterministic reports**: batch analysis and markdown reports are
  generated locally from the database — no extra AI calls.

## Requirements

- VS Code ≥ 1.85. macOS or Linux (local or Remote SSH). Windows not supported in V1.
- Go ≥ 1.27 to build the daemon (or use the bundled binary in the VSIX).
- Claude Code CLI for Claude runs; Codex CLI for Codex runs.
- Git for parallel batches.

## Install

```bash
npm install
npm run package        # builds daemon + webview + extension, produces the VSIX
code --install-extension agent-conductor-0.1.0.vsix
```

Then in VS Code: **Agent Conductor: Install Background Service**
(Claude/Codex awareness hooks are installed non-destructively at the same
time; your existing hooks are preserved).

### macOS

A LaunchAgent (`~/Library/LaunchAgents/local.agent-conductor.daemon.plist`)
runs the daemon with `RunAtLoad` + `KeepAlive`. The daemon cannot run while
the laptop is powered off; after sleep it resumes without duplicate anchors.

### Linux VPS (Remote SSH)

A systemd user service with `Restart=always`. For overnight survival after
SSH logout enable lingering once:

```bash
loginctl enable-linger $USER
```

(If that needs admin rights, run it yourself — Agent Conductor never uses
elevated privileges on your behalf.)

### Claude setup

1. Install the official Claude Code CLI and log in (`claude`).
2. Nothing else: managed runs use the interactive runtime in a daemon-owned
   PTY with your subscription. `ANTHROPIC_API_KEY` is stripped from managed
   children to protect subscription billing (advanced opt-out exists).

### Codex setup

1. Install the official Codex CLI and log in (`codex login`).
2. The daemon talks to `codex app-server` (JSON-RPC), sharing `CODEX_HOME`
   with the IDE extension. Models are discovered dynamically; plan/billing
   state comes from `account/read` — API-key billing triggers a loud warning.

## Shared Testing & Sudo Block — read first

The block you author (sudo password, test credentials, usage instructions)
is encrypted at rest, never logged, and — when attached to a run — **sent
literally to the provider, becoming part of that conversation history**. The
UI states this once, at first enable. Queued runs attach the latest version
at dispatch.

## Daily use

1. **QUEUE** — paste, choose, queue. `Run Next` inserts after the active run.
2. **BATCHES** — explicit multi-row orchestration via **Build Batch**
   (parallel rows need a clean Git tree; work lands on
   `conductor/bXX/integration`; your branch changes only when you press
   **Apply Batch to Current Branch**).
3. **CONTROL** — daemon health, provider status, honest quota bars, external
   busy sessions, Window Anchor, secret block, staged presets.

Run cards offer **Open in Claude Code** / **Open in Codex**, worktree
windows, effective-prompt copy (with secret warning), continue/retry/skip.

## Development

```bash
npm run daemon:test     # Go unit + scheduler scenario tests (fake providers)
npm test                # + extension/webview vitest suites
npm run lint            # typecheck both TS projects
npm run dev             # watch-build the extension; F5 for dev host
AC_MOCK=1 agent-conductor serve   # daemon with a fake provider (no real spend)
```

The build supports `darwin/arm64`, `darwin/amd64`, `linux/amd64`,
`linux/arm64` (`AC_RELEASE=1 bash scripts/build-daemon.sh`).

## Known limitations (honest list)

- Claude quota data has no structured read surface today: five-hour/weekly
  windows come from `StopFailure` + notification events (medium confidence);
  percentages show "unavailable" rather than invented numbers; a manual
  weekly-reset override exists and provider data always wins once available.
- Claude Window Anchor is **Experimental** for the same reason; Codex
  anchoring uses structured resets.
- Interactive Claude Code requires the workspace trust dialog to have been
  accepted; untrusted folders stop the run in `NEEDS_ATTENTION` with an
  explicit Trust Workspace action — trust is never accepted silently.
- Interactive Claude Code requires the workspace trust dialog to have been
  accepted; untrusted folders stop the run in `NEEDS_ATTENTION` with an
  explicit Trust Workspace action — trust is never accepted silently.
- Opening a specific Codex thread programmatically depends on documented
  public commands in the installed extension; otherwise the deterministic
  `[AC …]` title (copied for you) locates it in history instantly.
- Claude's URI handler for session resume is community-documented; the
  extension verifies the Claude extension is present first and falls back to
  copying the session ID.
- Agent Conductor cannot enforce account-level billing settings (usage
  credits / paid overage) — subscription-only operation is the intended
  default and API keys are stripped, but account-side spending is yours to
  control.
- Parallel batches require a clean Git repository and never touch your
  working tree without the explicit Apply action.
- macOS cannot pre-warm while the machine is powered off.
- Native provider behavior can change between releases; capability probes
  degrade gracefully and the capability matrix
  (`docs/provider-capabilities.md`) records confidence per integration.

## Uninstall

Command Palette → **Agent Conductor: Uninstall Background Service & Hooks**.
Runs and encrypted secrets stay under `~/.agent-conductor` until you delete
it manually.

## License

MIT.
