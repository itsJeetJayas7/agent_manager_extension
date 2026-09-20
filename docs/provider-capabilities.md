# Provider Capabilities Matrix

Research date: 2026-09-19. Verification pass against installed binaries:
2026-09-20. Agent Conductor verifies everything below at runtime through
capability detection; entries marked *inferred* are never treated as
guarantees.

> **Read this section first.** Several rows in the tables below were written
> from documentation and later contradicted by the installed CLIs. Where
> reality and this document disagreed, reality won and the code follows
> reality.

## Verification status, 2026-09-20

| Provider | Installed on the verification machine | Surface verified |
| --- | --- | --- |
| Claude Code | **yes** — `claude 2.1.278` | Headless stream-json protocol observed end to end, including the frame shapes below |
| Codex | **no** — not installed | **Nothing verified.** Every `codex app-server` method name below remains as documented, unconfirmed against a running binary |

### Claude Code: what the installed CLI actually does

Probed with:

```
echo "Reply with exactly: OK" | claude -p --output-format stream-json --verbose \
  --model haiku --permission-mode plan --tools ""
```

Frames observed, in order:

1. `{"type":"system","subtype":"init", session_id, model, permissionMode, tools, …}`
2. `{"type":"rate_limit_event","rate_limit_info":{…}}`
3. `{"type":"assistant","message":{model, content:[{type:"text"|"tool_use", …}]}}`
4. `{"type":"result","subtype":"success","is_error":false,"result":"…","stop_reason":"end_turn","num_turns":1,"session_id":"…","permission_denials":[],"terminal_reason":…,"api_error_status":…}`

**Divergence 1 — Claude DOES expose structured quota.** This document
previously recorded that exact reset timestamps were "not structurally
exposed", and the adapter's own comment said Claude "exposes no structured
quota read". Both were wrong. `rate_limit_event` carries:

```json
{"status":"allowed","resetsAt":1789887600,"rateLimitType":"five_hour",
 "unifiedWindows":{"five_hour":{"utilization":0.84,"resetsAt":1789887600},
                   "seven_day":{"utilization":0.06,"resetsAt":1790485200}}}
```

`utilization` is a fraction (0..1), `resetsAt` a Unix second. The adapter now
converts these to windows with `source: provider_structured` and
`confidence: exact`, and `ProbeQuota` answers from the newest observation
instead of returning an error. Text-derived windows remain only as a fallback
for the PTY path.

**Divergence 2 — the headless protocol is available and is now the default.**
`--print`, `--output-format stream-json`, `--input-format stream-json`,
`--resume <id>`, `--session-id <uuid>`, `--settings`, `--permission-mode`,
`--permission-prompts`, `--effort` and `--model` all exist in 2.1.278. The
adapter drove the interactive TUI through a PTY, slept 2500ms and pasted the
prompt as a bracketed-paste escape sequence; that path is now a fallback behind
a `--help` capability probe.

**Divergence 3 — `--permission-prompts none` is passed on the headless path.**
Nobody is present to answer a permission prompt in a daemon, so anything that
would prompt is denied rather than left hanging. Permission *mode* still
decides everything else.

**Divergence 4 — tool use is visible in the stream.** `MeaningfulProgress`
(which decides resume-vs-restart after a quota wait) is now derived from
`tool_use` blocks and tool results. It previously branched on `PreToolUse` /
`PostToolUse` hooks that `managedSettingsJSON()` never registered, so it could
only ever be set from a non-empty final message. Those hooks are registered
now as well, for the PTY path.

### Codex: what could NOT be verified

`codex` is not installed on the verification machine, so **none** of the
JSON-RPC method names in the Codex table below have been confirmed against a
running `codex app-server`. They are still as documented upstream.

Mitigations shipped instead of a verification:

- The adapter records the outcome of every JSON-RPC method it calls
  (`Codex.MethodSupport()`), so an unsupported method surfaces as a concrete
  error rather than silently doing nothing.
- `InstallCodex` no longer rewrites `~/.codex/config.toml`. It appends one
  delimited block and leaves every other byte alone, and refuses to write
  anything that would not parse. Whether Codex reads a `[hooks.*]` table at all
  is **unverified**; the block is delimited and removable precisely so that
  writing it is harmless if Codex ignores it.

**If you have Codex installed, verify before trusting the Codex column**:
`account/read`, `model/list`, `thread/start`, `thread/resume`, `turn/start`,
`turn/interrupt`, `account/rateLimits/read`, and the notification names
`item/agentMessage/delta`, `turn/completed`, `account/rateLimits/updated`.

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
| `-p --output-format stream-json` headless protocol | **Verified** on claude 2.1.278 | observed | **Default execution path.** Structured events: session init, assistant text, tool use, result with stop reason | interactive PTY path, behind a `--help` capability probe |
| VS Code URI handler `vscode://anthropic.claude-code/open?session=<id>` | Community-documented; **inferred** — attempted at runtime, degrades gracefully | VS Code marketplace docs | "Open in Claude Code" tries URI, verifies extension presence first | open command palette + show session ID |
| Five-hour + weekly limits; native quota auto-resume | **Verified structurally exposed** (2026-09-20): `rate_limit_event.unifiedWindows.{five_hour,seven_day}` with `utilization` and `resetsAt` | observed on claude 2.1.278 | Quota windows parsed from the stream; `source: provider_structured`, `confidence: exact` | `StopFailure` + notification text (PTY path), then the user-configured weekly reset override |
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
