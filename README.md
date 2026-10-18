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
  manually-started Claude/Codex sessions can run alongside it, including on
  the same provider. When you start an external turn during managed work,
  VS Code shows an informational notification with **Show queue** and
  **Pause queue…** actions. Opening a session does not trigger an alert.
  Disable these alerts with `agentConductor.notifyOnExternalOverlap`.
  New runs append to the end. Use the visible first/up/down/last controls,
  drag rows (including to the end), or press `Alt+↑/↓` to reorder.
  `Run Next` explicitly jumps the queue. Prompt-sequence dependencies stay in order.
- **Prompt sequences from a folder**: preview numbered Markdown prompts,
  choose their working folder, and import the entire sequence in one action.
  Each step waits for the previous step to succeed or be explicitly skipped.
- **Manual pause with a continuation timer**: hold the queue indefinitely or
  until a deadline you set (`15m`, `1h`, or anything you type like `1h30m`).
  The countdown is computed from an absolute timestamp, so it survives sleep,
  and the timer survives a daemon restart — including one that elapsed while
  the daemon was down. Optionally stop the run in progress too: it stops at a
  safe point and continues in the *same* native session on resume, never
  killed mid-tool-use. Quota exhaustion and configured quiet hours
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
  A stop hook requests a missing marker once. The scheduler can then resume
  the same session up to three times (configurable), including PLAN readiness,
  before `NEEDS_ATTENTION`. Explicit BLOCKED results require attention.
- **Automatic recovery**: network failures, provider overload and process
  crashes retry up to eight times with exponential delays from 5 seconds to
  5 minutes. Deadlines, prompts and native sessions survive daemon restarts;
  retries obey pause, quota and concurrency settings. Permanent auth, billing,
  permission, unavailable-model and context failures surface for attention.
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
- Claude Code CLI for Claude runs; Codex CLI for Codex runs. Opus 5.5 needs
  Claude Code 2.1.280+; Sonnet 5.5 needs 2.1.284+. Tested with Claude Code
  2.1.288 and Codex 0.160.0.
- Node.js 22.12+ for development and packaging.
- Git for parallel batches.

## Install

```bash
npm install
npm run package        # builds daemon + webview + extension, produces the VSIX
code --install-extension agent-conductor-0.3.1.vsix
```

Then in VS Code: **Agent Conductor: Install Background Service**
(Claude/Codex awareness hooks are installed non-destructively at the same
time; your existing hooks are preserved).

### macOS

A LaunchAgent (`~/Library/LaunchAgents/local.agent-conductor.daemon.plist`)
runs the daemon with `RunAtLoad` + `KeepAlive`. The daemon cannot run while
the laptop is asleep or powered off. Keep it awake for overnight execution;
after waking the scheduler resumes. A VPS avoids laptop sleep interruptions.

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
2. This personal installation trusts all queued working folders automatically.
   Select the explicit **Claude Opus 5.5** (`claude-opus-5-5`) model. The effort picker
   exposes low, medium, high, xhigh and max, plus the model default. Run cards
   show the provider-reported model and the effort used for the current stage.
   Staged Intelligence uses its own PLAN/BUILD/AUDIT presets in CONTROL;
   configure those too if every stage should use Opus 5.5.
3. Managed runs use Claude Code's headless protocol
   (`claude -p --output-format stream-json`) with your subscription, so the
   daemon sees structured events rather than screen-scraping a terminal. On a
   runtime without that protocol it falls back to the interactive PTY path
   automatically.
4. Subscription protection: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
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

1. **QUEUE** — paste, choose, queue at the end. `Run Next` runs next; use the
   first/up/down/last buttons, drag a row, or press `Alt+↑/↓` to reorder. The **Pause** button in the header holds
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

The [overnight validation report](docs/overnight-validation.md) records the
failure-handling matrix, measured load results and VPS deployment checklist.

### Import a prompt sequence

1. In **QUEUE**, click **Import prompts from folder**, or run **Agent
   Conductor: Import Prompts from Folder** from the Command Palette.
2. Select the folder containing your prompts. Expand any step to read its
   exact contents; fix reported problems and click **Refresh preview**.
3. Check the **Working folder**. This defaults to the open workspace (the
   first root in a multi-root workspace) and can be changed independently of
   the prompt folder. Agents make changes there.
4. Choose provider, model, effort, permissions, credentials and Staged
   Intelligence in the composer, then click **Queue N prompts in order**.
5. **View sequence** opens its batch detail with per-step status and the
   existing continue/retry/skip controls.

Use **Copy generator prompt** to get instructions you can append to a coding
agent's task. The folder format is deliberately simple:

```text
my-prompts/
  001-build-foundation.md
  002-add-feature.md
  003-verify-integration.md
```

Names use at least three digits, a hyphen, a lowercase kebab-case title, and
`.md`. Numbers must be unique and consecutive from 1; files sort numerically.
Each file's full UTF-8 text is one prompt, preserving whitespace and line
endings. No manifest or frontmatter is required. Each step starts a fresh
agent conversation, so include the context, prerequisites, constraints and
acceptance checks it needs; earlier results are available through files.

Only the selected folder's immediate regular files are imported. Other
entries are shown as ignored; malformed numbered Markdown filenames and
numbered symlinks/directories are errors. Limits: **500 steps, 256 KiB per
file, 8 MiB total**. A folder change after preview requires another review.
The last prompt folder is remembered per workspace. On Remote SSH, select a
folder on the remote host.

Import creates one atomic batch of sequential LINEAR rows, using the same
settings for every step. Failure, cancellation or an explicit blocker holds
later steps until resolved or skipped. Normal queue pauses, quiet hours,
quota limits and daemon recovery still apply. The daemon stores encrypted
copies of imported prompts; editing or removing source files afterward does
not alter queued work. This is an explicit import, not a folder watcher.
If an import response is lost, retry the same preview to recover the original
sequence and settings without queuing duplicates. Selecting a folder again
creates a new import. When upgrading, update/restart the background service
as well as the extension; an older daemon cannot import sequences.

## Development

```bash
npm run daemon:test     # Go unit + scheduler scenario tests (fake providers)
npm test                # + extension/webview vitest suites
npm run lint            # typecheck both TS projects
npm run test:e2e        # real daemon + real IPC, fake provider (free, in CI)
npm run test:live       # real Claude Opus 5.5 + Codex + native Claude continuation
npm run test:stress     # 100 free fake runs + 500 concurrent snapshot requests
npm run test:browser    # built UI in installed Chrome (CHROME_PATH override)
npm run test:sequence   # real folder import + daemon restart; fake execution
npm run dev             # watch-build the extension; F5 for dev host
AC_MOCK=1 agent-conductor serve   # daemon with a fake provider (no real spend)
```

`test:live` is opt-in: it needs both CLIs installed and signed in, and it
spends real quota. It queues one Claude run and one Codex run against the same
file in a throwaway git repo, asserts the two never executed at once, and
continues Claude in its original session. It checks that all three signed
lines are present and correctly numbered — a
lost update or a concurrent dispatch fails the test. The daemon's state goes
in a scratch `AC_ROOT`, never your real `~/.agent-conductor`.

The build supports `darwin/arm64`, `darwin/amd64`, `linux/amd64`,
`linux/arm64` (`AC_RELEASE=1 bash scripts/build-daemon.sh`).

## One daemon per state directory

Exactly one daemon serves `~/.agent-conductor` at a time, enforced with an
advisory lock. A second `agent-conductor serve` against the same root exits
immediately and says who holds it — it does not take over the socket while the
first keeps scheduling. Use `AC_ROOT` to run a genuinely separate instance.

This personal installation supports VS Code's **Restricted Mode** and assumes
all queued working folders are trusted. No separate folder approval is needed.
Provider permission choices still apply. Runs blocked solely by the former
trust gate rejoin the queue on upgrade, keeping their position and honoring
existing pauses.

## Known limitations (honest list)

- Claude quota windows are learned from the first managed run: the
  `rate_limit_event` frames arrive during a run, not from a standalone query.
  Before that first run the Control tab says so rather than guessing. A manual
  weekly-reset override exists and provider data always wins once available.
- The **Codex** integration is now verified against an installed
  `codex-cli 0.160.0`: the JSON-RPC surface was checked against the binary's
  own schema (`codex app-server generate-json-schema`) and driven end to end
  by `npm run test:live`. Whether Codex reads a `[hooks.*]` table at all is
  still unverified — Agent Conductor appends one clearly delimited, removable
  block and touches nothing else in your `config.toml`, so it is harmless if
  ignored. External-Codex awareness degrades to `unknown` in that case. See
  `docs/provider-capabilities.md`.
- Both CLIs are often installed **only** inside their official VS Code
  extension and never land on `PATH`. Agent Conductor looks there too (VS
  Code, Insiders, the remote server, Cursor, Windsurf, VSCodium) after `PATH`
  and the usual install locations. If your install is somewhere stranger, set
  `AC_CODEX_BIN` / `AC_CLAUDE_BIN`; `agent-conductor diagnostics` prints the
  path it resolved.
- Opening a specific Codex thread programmatically depends on documented
  public commands in the installed extension; otherwise the deterministic
  `[AC …]` title (copied for you) locates it in history instantly.
- **Open in Claude Code** shows the run live in Claude Code's own UI: the
  original prompt, history, then new messages, tool calls and completion as
  they happen. The Claude Code tab attaches to the process the daemon is
  running (through Claude Code's `claudeProcessWrapper` setting, enabled after
  you confirm once) instead of starting a second one; Stop, follow-up messages
  and Conductor's own continuations all go to that one process. Conductor says
  "attached" only after the daemon sees the tab attached. Worktree sessions
  open in their own window. See [live view](docs/claude-native-live-view.md)
  for how it works and its limits; **Disable Live View in Claude Code** turns
  it off.
- **Live activity** reports streamed output, last provider event age and process
  liveness independently of the saved run state. A live process with no output
  is shown as quiet, not confirmed progress. Recent activity is kept only in
  daemon memory (100 entries per run, up to 100 recent runs); it resets on daemon
  restart and on each continuation. Native sessions retain full history.
- Agent Conductor cannot enforce account-level billing settings (usage
  credits / paid overage) — subscription-only operation is the intended
  default and API keys are stripped, but account-side spending is yours to
  control.
- Parallel batches require a clean Git repository and never touch your
  working tree without the explicit Apply action.
- macOS cannot run agents while the machine is asleep or powered off.
- Native provider behavior can change between releases; capability probes
  degrade gracefully and the capability matrix
  (`docs/provider-capabilities.md`) records confidence per integration.

## Uninstall

Command Palette → **Agent Conductor: Uninstall Background Service & Hooks**.
Runs and encrypted secrets stay under `~/.agent-conductor` until you delete
it manually.

## License

MIT.
