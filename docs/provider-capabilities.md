# Provider Capabilities Matrix

Research date: 2026-09-19. Verified against official documentation where possible.
Agent Conductor verifies everything below at runtime through capability detection;
entries marked *inferred* are never treated as guarantees.

## Claude Code (Anthropic)

| Capability | Status | Source | Chosen implementation | Fallback |
| --- | --- | --- | --- | --- |
| Hooks: `SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`, `SessionEnd`, `Notification` | Documented (settings `hooks` map, matcher + command list) | https://code.claude.com/docs/en/hooks | Namespaced entries merged into `~/.claude/settings.json` forwarding to `agent-conductor hook claude` | none needed |
| Stop hook can block stopping (`decision:"block"`, `reason`) | Documented | hooks reference | Completion gate emits block JSON once when marker missing | allow stop, mark `NEEDS_ATTENTION` |
| `stop_hook_active` field on Stop input (loop guard; 8-block cap) | Documented | hooks reference | Gate always exits 0 when `stop_hook_active` is true | — |
| `StopFailure` matcher values: `rate_limit`, `overloaded`, `authentication_failed`, `billing_error`, `model_not_found`, `server_error`, … | Documented | hooks reference | Structured error classification; `rate_limit` drives quota wait | text parsing (fallback only) |
| Hook common input: `session_id`, `transcript_path`, `cwd`, `model` | Documented | hooks reference | Managed-session identity + external awareness | — |
| `Notification` types incl. quota auto-resume (`quota_auto_resume_fired/stale/disabled` family) | Documented *family*; exact type strings **inferred** — verified at runtime by substring match | hooks reference (notification_type matcher) | When native auto-resume fires, Conductor does NOT send a duplicate continue | Conductor-owned resume on stale/disabled |
| CLI `--permission-mode` incl. `bypassPermissions`, `plan` | Documented | https://code.claude.com/docs/en/cli-reference | Managed runs launch with selected mode; default `bypassPermissions` | fail clearly if provider forbids |
| CLI `--resume <session-id>` | Documented | cli reference; sessions guide | Same-session continuation after quota reset | new attempt only if resume fails and no progress |
| CLI `--settings <file\|json>` | Documented | cli reference | Managed per-run settings incl. hooks | — |
| CLI `--session-id <uuid>` | Documented | cli reference | Deterministic session ID chosen by daemon; persisted before launch | capture from `SessionStart` hook |
| CLI `--model`, `--effort` | Documented (`--effort` used by `claude agents` dispatch) | cli reference | Passed through; capability-probed via `claude --help` at setup | omit flag |
| `-p`/SDK sessions not in picker but resumable by ID | Documented | sessions guide | We launch **interactive in a daemon-owned PTY** (spec), so sessions appear normally | — |
| VS Code URI handler `vscode://anthropic.claude-code/open?session=<id>` | Community-documented; **inferred** — attempted at runtime, degrades gracefully | VS Code marketplace docs | "Open in Claude Code" tries URI, verifies extension presence first | open command palette + show session ID |
| Five-hour + weekly limits; native quota auto-resume | Documented behaviorally (plan limits), exact reset timestamps **not** structurally exposed | Anthropic plan docs | Quota windows from `StopFailure` + notification text; confidence `high`/`medium`; never fake percentages | user-configured weekly reset override |
| `ANTHROPIC_API_KEY` in env may switch billing away from subscription | Documented risk pattern | CLI env docs | Stripped from managed subprocess env unless API mode explicitly enabled | — |

## Codex (OpenAI)

| Capability | Status | Source | Chosen implementation | Fallback |
| --- | --- | --- | --- | --- |
| `codex app-server` — JSON-RPC 2.0 over stdio (header omitted) | Documented | https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md | Long-lived child process owned by daemon | — |
| `initialize` handshake with `clientInfo` | Documented | app-server README | performed at daemon start | — |
| `thread/start` (params: `model`, `cwd`, `approvalPolicy`, `sandbox`, `serviceName`) → thread + `thread/started` | Documented | app-server README | Managed Codex runs; thread ID persisted immediately | — |
| `thread/resume <id>`, `thread/read`, `thread/list` | Documented | app-server README / MCP interface doc | crash reconciliation, native visibility | — |
| `turn/start` (params: `threadId`, `input[]`, `cwd`, `model`, `effort`, `sandboxPolicy`, `approvalPolicy`) | Documented | app-server README | prompt dispatch + staged intelligence model switch per turn | — |
| `turn/interrupt`, `turn/steer` | Documented | app-server README | cancellation | process-level interrupt |
| Notifications `turn/started`, `item/started`, `item/agentMessage/delta`, `item/completed`, `turn/completed` with status `completed\|interrupted\|failed` | Documented | app-server README | authoritative completion state; final agent message assembled from `item/agentMessage` | — |
| `model/list` dynamic catalog (id, display name, efforts) | Documented | MCP interface doc | dynamic model/effort pickers — nothing hardcoded | — |
| `account/read` (auth mode, plan) | Documented | MCP interface doc | API-key billing → loud warning | — |
| `account/rateLimits/read` + `account/rateLimits/updated` events | Documented | MCP interface doc / spec | Quota Guardian primary source, confidence `exact` | confidence `unknown` shown honestly |
| Bypass permissions: `approvalPolicy: "never"` + sandbox `dangerFullAccess` (legacy shorthand `"sandbox"` values include full access) | Documented | app-server README (approvalPolicy/sandbox) | translated "Bypass" run mode | `workspaceWrite` + warning |
| Thread naming API | **Not documented** in current app-server surface | — | Deterministic `[AC Bxx-Ryy] title` prefix embedded in thread history + serviceName `agent_conductor`; exact thread open in IDE not claimed | copy thread title/ID to locate in native history |
| Codex hooks (`SessionStart`, `UserPromptSubmit`, `Stop`, `Interrupt`, `SessionEnd` in `config.toml`) | Documented as hook support; schema **experimental** | codex docs (hooks) | Namespaced `[hooks.*]` merge, verified after install; awareness-only | external Codex detection degrades to `unknown` |

## VS Code

| Capability | Status | Chosen implementation |
| --- | --- | --- |
| `WebviewView` + `WebviewPanel`, strict CSP + nonce | Documented | sidebar + batch planner/editor-area panels; all assets bundled, no CDN |
| `extensionKind: workspace` for Remote SSH | Documented | extension runs on remote host where daemon/git/providers live |
| `SecretStorage` | Documented | not used for prompt secrets (daemon vault owns them); reserved for future |
| `vscode.env.openExternal`, `vscode.open`, `vscode.openFolder` | Documented | URI openers, worktree windows |
| `vscode.commands.getCommands` | Documented | capability detection for Codex/Claude extension commands (documented-only usage) |
| VSIX packaging via `@vscode/vsce` | Documented | `npm run package` |

## Version constraints

- Claude Code CLI: verified at runtime (`claude --version`, `claude --help` flag probe). Hooks require current CLI generations (2026 line).
- Codex CLI: `codex app-server` required; detected via `codex --version` + spawn probe.
- VS Code >= 1.85 for contributed webview views without activation events.
