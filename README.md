# Agent Conductor

Queue, supervise, and orchestrate **Claude Code** and **Codex** coding-agent
runs from one quiet VS Code sidebar — then let them run unattended. A
persistent daemon owns execution: closing VS Code, sleeping your laptop, or
dropping the SSH connection does **not** kill managed work. Every managed run
keeps its native provider session/thread so you can always inspect it in the
official Claude Code / Codex extensions.

```text
VS Code sidebar ── Unix socket ── Conductor daemon ── Claude (stream-json) / Codex (app-server)
                                       │
                              SQLite (WAL) + AES-256-GCM vault + quota guardian
```

## What it does

- **Queue composer**: paste a prompt verbatim (no AI rewriting), pick
  provider / model / effort / permission — remembered per workspace — attach
  the encrypted **Shared Testing & Sudo Block**, enable **Staged
  Intelligence**.
- **FIFO queue with awareness**: one managed run at a time by default;
  manually-started Claude/Codex sessions block the queue (never killed).
  `Run Next` jumps the queue, and rows can be dragged or moved with
  `Alt+↑/↓`.
- **Manual pause with a continuation timer**: hold the queue indefinitely or
  until a deadline you set (`15m`, `1h`, or anything you type like `1h30m`).
  The countdown is computed from an absolute timestamp, so it survives sleep,
  and the timer survives a daemon restart — including one that elapsed while
  the daemon was down. Optionally stop the run in progress too: it stops at a
  safe point and continues in the *same* native session on resume, never
  killed mid-tool-use. Quota exhaustion, an external agent and quiet hours
  reuse the same machinery, so there is one place that answers "why is
  nothing running?".
- **Quiet hours**: outside the hours you choose the queue holds itself, with
  the same countdown to the next working window.
- **Batches**: LINEAR and PARALLEL rows; parallel cards run concurrently in
  isolated **Git worktrees**, integrate into an internal branch, and barriers
  hold the next row until every card succeeds. Failures block subsequent rows
  until retried or explicitly skipped.
- **Completion contract**: managed runs must end with
  `<AC_RESULT>COMPLETE</AC_RESULT>` or `<AC_RESULT>BLOCKED: reason</AC_RESULT>`.
  A stop hook blocks the first marker-less stop once; the second lands in
  `NEEDS_ATTENTION` — no infinite loops, external sessions never gated.
- **Quota guardian**: five-hour and weekly windows normalized per provider,
  both from structured provider data — Codex `account/rateLimits`, Claude
  `rate_limit_event` frames in the stream — at `exact` confidence. When a
  value genuinely is unknown the UI says "unavailable" rather than drawing a
  bar. Runs wait, then resume the **same native conversation** after the max
  of all exhausted resets.
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
- **Survives restarts for real**: a daemon restart reattaches to each run's
  native session rather than handing it back to you.
- **Tells you what happened**: on reconnect you get one notification
  summarising what finished while you were away.
- **Measured, not guessed**: a usage summary of wall-clock active time, quota
  waits and continuation counts. No invented dollar figures.

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
2. Nothing else. Managed runs use Claude Code's headless protocol
   (`claude -p --output-format stream-json`) with your subscription, so the
   daemon sees structured events rather than screen-scraping a terminal. On a
   runtime without that protocol it falls back to the interactive PTY path
   automatically.
3. Subscription protection: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
   `ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL`, the Bedrock/Vertex switches and
   `AWS_*`/`GOOGLE_*`/`AZURE_*` credentials are all stripped from managed
   children so a run cannot bill somewhere you did not intend. There is an
   advanced opt-out.

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

1. **QUEUE** — paste, choose, queue. `Run Next` really does run next; drag a
   row or press `Alt+↑/↓` to reorder. The **Pause** button in the header holds
   the queue, with or without a timer.
2. **BATCHES** — explicit multi-row orchestration via **Build Batch**
   (parallel rows need a clean Git tree — uncommitted *tracked* changes
   block, untracked files only warn; work lands on
   `conductor/bNNN-xxxxxx/integration`, a collision-free name; your branch
   changes only when you press **Apply Batch to Current Branch**, and a
   `conductor/backup/<timestamp>` ref is taken first).
3. **CONTROL** — an always-visible status summary (daemon, providers, quota),
   then progressive disclosure for provider detail, scheduling and quiet
   hours, Window Anchor, the secret block, staged presets and maintenance.

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

- Claude quota windows are learned from the first managed run: the
  `rate_limit_event` frames arrive during a run, not from a standalone query.
  Before that first run the Control tab says so rather than guessing. A manual
  weekly-reset override exists and provider data always wins once available.
- The **Codex** integration has not been verified against an installed
  `codex` binary. Every JSON-RPC method name comes from upstream
  documentation; the adapter records which ones actually work and surfaces
  failures instead of failing silently. Whether Codex reads a `[hooks.*]`
  table at all is unverified — Agent Conductor appends one clearly delimited,
  removable block and touches nothing else in your `config.toml`. See
  `docs/provider-capabilities.md`.
- Agent Conductor asks you to approve a folder once before running agents in
  it. Claude Code's headless mode skips its own trust dialog, so this is
  Agent Conductor's own consent gate rather than a provider requirement: an
  unapproved folder stops the run in `NEEDS_ATTENTION` with a one-click Trust
  action. It is never accepted on your behalf.
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
