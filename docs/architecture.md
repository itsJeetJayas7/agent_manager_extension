# Architecture

```text
                   VS CODE
                     │
            ┌────────┴────────┐
            │ Agent Conductor │  (TypeScript extension, extensionKind: workspace)
            │     sidebar     │  webview React app, strict CSP, nonce
            └────────┬────────┘
                     │  postMessage → extension host → Unix socket NDJSON (token auth)
             ┌───────▼────────┐
             │ Conductor      │  Go daemon (systemd --user / LaunchAgent)
             │ background     │  SQLite WAL state · scheduler · quota guardian
             │ daemon         │  vault (AES-256-GCM) · hook bridge · anchors
             └───────┬────────┘
        ┌────────────┴─────────────┐
  Claude adapter                Codex adapter
  interactive PTY               `codex app-server` JSON-RPC
  lifecycle hooks               thread/turn events
        │                            │
  native Claude session         native Codex thread
        │                            │
  official Claude UI            official Codex UI
```

## Principles

- **The daemon owns execution.** VS Code closing, SSH disconnects, and
  extension-host crashes never kill a managed run. The webview is a
  projection: all authoritative state lives in SQLite.
- **Provider-native identity.** Every run stores its Claude session ID or
  Codex thread ID; conversations remain inspectable in the official
  extensions.
- **Events over timers.** Completion comes from provider lifecycle events
  (hooks / `turn/completed`), never from silence heuristics.
- **Explicit state.** All 21 run states are persisted and transition-validated.

## Daemon layout

| Package | Responsibility |
| --- | --- |
| `internal/model` | entities, state machine, prompt assembly, markers, error taxonomy |
| `internal/db` | SQLite (modernc, CGO-free), migrations, transactional transitions |
| `internal/vault` | AES-256-GCM encryption of prompts and the shared secret block |
| `internal/logging` | JSON logs with pre-write redaction |
| `internal/providers` | Adapter interface; Claude PTY adapter, Codex app-server adapter, deterministic fake |
| `internal/quota` | window normalization, max-of-exhausted next-eligible |
| `internal/scheduler` | FIFO queue, slots, external busy, rows/barriers, worktrees, staged intelligence, verification, anchors, crash recovery |
| `internal/worktrees` | git isolation, integration merges, conflict handling, guarded apply |
| `internal/hooks` | non-destructive provider hook merge (namespaced, backup, idempotent) |
| `internal/hooksbridge` | the `hook claude\|codex` subcommand incl. completion gate |
| `internal/api` | Unix socket protocol v1 (NDJSON, 0600, token) |
| `internal/service` | systemd user unit / LaunchAgent install |

## IPC protocol (v1)

First line: `hello {protocol, auth}`. Then requests
`{protocol, id, method, params}` → `{id, result}` or `{id, error:{code,message}}`.
Events pushed after `subscribe`: `snapshot`, `runUpdated`, `batchUpdated`,
`quotaUpdated`, `externalSessionUpdated`, `settingsUpdated`, `worktreeUpdated`.

The webview never opens sockets; routing is always
webview → extension host → daemon.
