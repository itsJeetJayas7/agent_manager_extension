# Provider Capabilities Matrix

Research date: 2026-09-19. Verification pass against installed binaries:
2026-09-20. Agent Conductor verifies everything below at runtime through
capability detection; entries marked *inferred* are never treated as
guarantees.

> **Read this section first.** Several rows in the tables below were written
> from documentation and later contradicted by the installed CLIs. Where
> reality and this document disagreed, reality won and the code follows
> reality.

## Verification update, 2026-10-03

Session opening was also checked in an isolated VS Code extension host against
Claude Code 2.1.288: the contributed command loaded a synthetic saved conversation
under its expected title, and a second invocation focused the same tab. This
check used the existing sign-in and did not submit a provider turn. Remote
workspace routing and new-window handoff replay are covered by extension tests;
a live Remote SSH window was not available for this check. Live monitoring is
provided by Conductor's daemon activity endpoint, not an assumed native UI
attachment to the running CLI process.

Claude Code 2.1.288 and Codex 0.160.0 passed the live daemon integration test.
Claude reported `claude-opus-5-5` while making a real file edit at `xhigh`,
then resumed the same native session for another edit. Codex edited the same
file serially. All five Opus 5.5 effort values are also checked against the
adapter's generated CLI arguments, including resumed turns; this is not five
separate paid live runs.

The Claude catalog uses explicit version IDs plus clearly labeled moving
aliases. Availability is account-dependent. Model-specific effort ladders
follow the [Claude Code model configuration](https://code.claude.com/docs/en/model-config)
and [Anthropic effort reference](https://platform.claude.com/docs/en/build-with-claude/effort).
Opus 5.5 requires CLI 2.1.280+; Sonnet 5.5 requires 2.1.284+.
`allowed_warning` quota frames no longer imply exhaustion. Run cards record
provider-reported model IDs instead of treating the requested model as proof.

The sections below preserve the earlier protocol observations. Current test
coverage and limitations are in [overnight-validation.md](overnight-validation.md).

## Verification status, 2026-09-20

| Provider | Installed on the verification machine | Surface verified |
| --- | --- | --- |
| Claude Code | **yes** — `claude 2.1.278` | Headless stream-json protocol observed end to end, including the frame shapes below |
| Codex | **yes** — `codex-cli 0.154.0-alpha.6.2`, bundled inside the `openai.chatgpt` VS Code extension | Verified against the binary's own JSON Schema and by driving two live runs end to end; see "Codex: verified 2026-09-20" |

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

**Divergence 3 — `--permission-prompts none` is capability-gated.**
The adapter passes this optional flag only when the installed CLI advertises
it in `--help`. It requires Claude Code 2.1.259+; older VPS installations
support stream-json but reject the flag before starting the turn. Those
installations omit it and use print mode without a permission host. The
selected permission mode is preserved for both new and resumed turns.
See [unattended permission prompts](https://code.claude.com/docs/en/headless#turn-off-permission-prompts-in-unattended-runs)
for how requests without a permission host are denied.

**Divergence 4 — tool use is visible in the stream.** `MeaningfulProgress`
(which decides resume-vs-restart after a quota wait) is now derived from
`tool_use` blocks and tool results. It previously branched on `PreToolUse` /
`PostToolUse` hooks that `managedSettingsJSON()` never registered, so it could
only ever be set from a non-empty final message. Those hooks are registered
now as well, for the PTY path.

### Codex: verified 2026-09-20

`codex` **is** installed on this machine, but not on `PATH`: the OpenAI VS Code
extension ships it at

```
~/.vscode/extensions/openai.chatgpt-<version>/bin/<platform-triple>/codex
```

Claude Code does the same at
`~/.vscode/extensions/anthropic.claude-code-<version>/resources/native-binary/claude`.
`findBinary` therefore searches the extension directories of VS Code, VS Code
Insiders, the remote server, Cursor, Windsurf and VSCodium after PATH and the
usual install locations, preferring the most recently modified copy.
`AC_CODEX_BIN` / `AC_CLAUDE_BIN` override everything. Before this, a fully
working, signed-in Codex install reported **"runtime missing"** in the sidebar.

The protocol surface was verified against ground truth rather than docs:

```
codex app-server generate-json-schema --out <dir>
```

which emits the installed binary's own `ClientRequest` / response schemas. All
methods the adapter calls exist there — `initialize`, `account/read`,
`model/list`, `account/rateLimits/read`, `thread/start`, `thread/resume`,
`turn/start`, `turn/interrupt`. Four payload divergences were found and fixed:

**Divergence 1 — the sandbox enum is spelled two different ways.**
`thread/start` takes `sandbox`, a `SandboxMode` **string** in kebab-case
(`read-only` | `workspace-write` | `danger-full-access`). `turn/start` takes
`sandboxPolicy`, a tagged **object** whose `type` is camelCase (`readOnly` |
`workspaceWrite` | `dangerFullAccess`). The adapter sent the camelCase tag as
`sandbox` to both, so every real Codex run failed at `thread/start` with:

```
Invalid request: unknown variant `dangerFullAccess`,
expected one of `read-only`, `workspace-write`, `danger-full-access`
```

**Divergence 2 — `model/list` returns `{data: [...]}`, not `{models: [...]}`.**
The model picker was always empty. Each `Model` carries
`supportedReasoningEfforts` and `defaultReasoningEffort`, so the effort ladder
now comes from the model rather than from a hardcoded list.

**Divergence 3 — `account/read` returns `{account: {type, planType, email},
requiresOpenaiAuth}`**, not a flat `{authMode, plan}`. Auth state therefore
always displayed as "unknown" for a signed-in account. `type` is `chatgpt` |
`apiKey` | `amazonBedrock`; a null `account` means nobody is signed in.

**Divergence 4 — capability reporting conflated two failures.** `Installed`
was only set after the app-server handshake succeeded, so a handshake failure
was reported to the user as a missing runtime. The binary's presence and the
app-server's health are now reported separately.

Confirmed working against the live binary: `approvalPolicy` values
(`untrusted` | `on-request` | `never`), `turn/start` input items
(`{"type":"text","text":...}`), `account/rateLimits/read` (returns
`{rateLimits: {primary, secondary}}` with `usedPercent` / `resetsAt` /
`windowDurationMins`, giving Codex `exact` quota confidence), and thread IDs
surviving as the native session for inspection in the Codex extension.

`tests/integration/providers-live.mjs` exercises all of this against the real
binaries (`npm run test:live`), and `TestPermissionToCodexUsesTheProtocolSpellings`
pins the enum spellings so a regression fails in unit tests rather than on a
user's first real run.

Still **unverified**: whether Codex reads a `[hooks.*]` table in
`~/.codex/config.toml` at all. `InstallCodex` appends one delimited, removable
block and leaves every other byte alone, so writing it is harmless if Codex
ignores it. External-Codex detection degrades to `unknown` if it does.

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
| Native session opening | Claude 2.1.288 contributed commands accept the native session ID; activated and capability-checked | Installed extension; [documented workspace-scoped resume](https://code.claude.com/docs/en/vs-code#launch-a-vs-code-tab-from-other-tools) | Current-host command, with a new-window handoff for a different cwd/worktree | Actionable warning and copied resume command; Live activity remains available |
| Live native view of a running run | **Documented seam:** `claudeCode.claudeProcessWrapper`. **Version-specific:** extension hides `sdk-*` sessions; SDK control protocol; `CLAUDE_CODE_ENTRYPOINT` honored in print mode except `cli`; per-turn `system/init`; `isReplay` echoes with caller uuid | Installed 2.1.288 extension and CLI, live A/B tests ([details](claude-native-live-view.md)) | Daemon-owned stream-input process per run; wrapper relays the tab to it; attach confirmed by the daemon | Tab opens the saved conversation and the user is told it will not update |
| Five-hour + weekly limits; native quota auto-resume | **Verified structurally exposed** (2026-09-20): `rate_limit_event.unifiedWindows.{five_hour,seven_day}` with `utilization` and `resetsAt` | observed on claude 2.1.278 | Quota windows parsed from the stream; `source: provider_structured`, `confidence: exact` | `StopFailure` + notification text (PTY path), then the user-configured weekly reset override |
| `ANTHROPIC_API_KEY` in env may switch billing away from subscription | Documented risk pattern | CLI env docs | Stripped from managed subprocess env unless API mode explicitly enabled | — |

## Codex (OpenAI)

| Capability | Status | Source | Chosen implementation | Fallback |
| --- | --- | --- | --- | --- |
| `codex app-server` — JSON-RPC 2.0 over stdio (header omitted) | Documented | https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md | Long-lived child process owned by daemon | — |
| `initialize` handshake with `clientInfo` | Documented | app-server README | performed at daemon start | — |
| `thread/start` (params: `model`, `cwd`, `approvalPolicy`, `sandbox` as a kebab-case `SandboxMode`, `serviceName`) → thread + `thread/started` | **Verified** against the installed binary's JSON Schema and a live run | `codex app-server generate-json-schema` | Managed Codex runs; thread ID persisted immediately | — |
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
- Codex CLI: `codex app-server` required; detected via `codex --version` + spawn probe. Discovery covers PATH, the usual install locations and editor extension bundles (`openai.chatgpt`), with `AC_CODEX_BIN` as an override.
- VS Code >= 1.85 for contributed webview views without activation events.
