# Showing a managed Claude run live in Claude Code

Verified with Claude Code 2.1.288 (CLI and VS Code extension), VS Code 1.138,
macOS arm64. Everything marked *version-specific* below was observed in that
build and is not part of a documented contract.

## Why "Open in Claude Code" opened a fresh conversation

Two independent causes, both reproduced with a real daemon-owned run.

**1. Managed sessions were invisible to the extension.** In print mode
(`claude -p`) the CLI tags every transcript entry `"entrypoint":"sdk-cli"`.
The extension's conversation list for VS Code deliberately omits programmatic
sessions (`sdk-cli`, `sdk-ts`, `sdk-py`; a hard-coded
`includeProgrammaticSessions = false` in the VS Code host). When the extension
is asked to open a session it cannot find, it starts a fresh conversation, as
documented: *"If the session isn't found, a fresh conversation starts
instead."* A/B test: the identical transcript with only `entrypoint` changed to
`cli` opened with full history; tagged `sdk-cli` it opened empty. The earlier
verification used a synthetic transcript that was not tagged `sdk-cli`, which
is why it passed.

**2. Even a findable session is never followed live.** The extension reads the
transcript once when a tab opens (`get_session_request`), then can only
continue the conversation by spawning its own `claude --resume <id>` process.
If another process holds the session it shows *"This conversation is still open
somewhere else… Open here anyway"*; choosing that starts a second process on
the same conversation (observed: a bundled `native-binary/claude --resume=<id>`
next to the original). A running interactive session opened this way stayed
frozen at its first tool call while the process finished all its steps.

Sharing a session ID or transcript file is therefore not an attachment.

## What does attach

The extension runs every conversation's process through
`claudeCode.claudeProcessWrapper` when it is set (documented: *"Executable used
to launch the Claude process. The bundled binary path is passed as an
argument"*). It speaks the Agent SDK stream-json protocol to that process over
stdin/stdout, and with a wrapper it skips its "held elsewhere" check.

Agent Conductor uses that seam without adding a second execution:

```
Conductor daemon ──owns──▶ claude -p --input-format stream-json --output-format stream-json
       ▲                         --replay-user-messages --session-id <id>   (one process per run)
       │ attachClaudeSession (Unix socket, token auth)
agent-conductor claude-wrapper ◀── stdin/stdout ──▶ Claude Code tab
   (claudeCode.claudeProcessWrapper)
```

- **One owner.** The daemon starts each run's Claude process in streaming-input
  mode and writes prompts to it as SDK user messages. Continuation turns of the
  run go into the same live process, so an attached tab follows them.
- **Attach instead of spawn.** When the extension launches a conversation with
  `--resume <id>`, the wrapper asks the daemon. For a managed session it relays
  the tab to the daemon-owned process; if the run has no process at that moment
  (between turns, completed, after a VS Code restart) the daemon starts one
  with `--resume` and the run's own model and permission settings, so the
  daemon remains the only owner. Every other launch is replaced, unchanged, by
  the real `claude` (`exec`). A managed session already held by another Claude
  process (per Claude's `~/.claude/sessions/<pid>.json` registry) is refused
  rather than forked.
- **Protocol relay.** The tab's `initialize` is answered from the response the
  process gave the daemon at start (a second `initialize` would be refused).
  The tab's control requests are forwarded with rewritten ids and their
  responses routed back. Messages typed in the tab, Stop (`interrupt`), model
  and mode changes go to the same process. Output is broadcast to every
  attached tab.
- **Honest labeling.** Managed processes run with
  `CLAUDE_CODE_ENTRYPOINT=agent-conductor` so their transcripts are listed. The
  CLI honors this variable in print mode for values other than `cli`
  (*version-specific*, undocumented).
- **Exact continuity at attach.** A tab shows the transcript as read when it
  opens and attaches 50 ms to several seconds later; the extension does not
  de-duplicate. When Conductor opens the tab it asks the daemon to pause the
  Claude CLI process (SIGSTOP, not its tool processes) for at most 8 s; the
  tab's `initialize` resumes it after the daemon replays exactly the output not
  yet in the transcript. Focusing an already attached tab never pauses.
- **Turn attribution.** Prompts carry a uuid; the CLI's `isReplay` echo marks
  when Conductor's turn starts. Results of turns started from the tab, or by
  Claude itself (background task completion), are not reported as the run's
  turn, and a Conductor prompt waits until they finish.

## Behavior

| Action | Result |
| --- | --- |
| Open in Claude Code (running run) | Native tab with the original prompt and history; new tool calls, results and completion stream in. Conductor reports success only after the daemon sees the tab attached. |
| Click again | Focuses the same tab; no new process, attachment or pause. |
| Close the tab / VS Code | Detaches only; the run continues in the daemon. Reopening reattaches. |
| Stop in Claude Code | Interrupts the turn; run becomes FAILED / USER_INTERRUPT "Stopped from Claude Code". The process stays for the tab. |
| Message typed in Claude Code | Runs in the same process as a native turn; not reported as the run's turn. |
| Cancel in Conductor | Interrupts, ends the process and its tool processes; the tab detaches. |
| Continue in Conductor | Written into the same process; appears live in an attached tab. |
| Idle process, no tab | Ended after 2 minutes; the next turn or tab starts one with `--resume`. |

## Enabling and disabling

The first **Open in Claude Code** asks before setting the wrapper. The setting
is machine-scoped in the Claude Code extension, so it applies to every Claude
Code conversation on that machine (other conversations start exactly as
before, through `exec`). Commands: **Agent Conductor: Enable / Disable Live
View in Claude Code**. A wrapper the user configured is never replaced.
Uninstalling the background service removes the setting.

The setting names `~/.agent-conductor/bin/agent-conductor-claude-wrapper`, a
shell script. It probes each candidate daemon binary for wrapper support and,
if none is installed, starts Claude unchanged, so removing Agent Conductor
cannot stop Claude Code from launching. The extension also removes the setting
on startup if the script is missing.

Documented side effects of any wrapper (code.claude.com/docs/en/vs-code): the
extension skips its "open somewhere else" check, and new conversations start in
Manual mode unless `claudeCode.initialPermissionMode` is set.

## Limitations

- **Version-specific dependencies:** the SDK control protocol between the
  extension and the CLI, the extension filtering `sdk-*` entrypoints, the
  `CLAUDE_CODE_ENTRYPOINT` override and the per-turn `system/init` and
  `isReplay` echoes are implementation details of 2.1.288. Conductor reports
  live only when the daemon sees a tab attached, so a change shows as "did not
  attach", not as a false success.
- **Requires the updated daemon.** An older running daemon cannot relay; the
  extension says to run **Install Background Service**.
- **Hooks and IDE tools the tab registers** (diff previews, diagnostics MCP)
  are not added to a process the daemon already started; the run uses the
  settings it was started with.
- **A tab restored by VS Code** (not opened through Conductor) attaches without
  the pause, so a message written in the instant between its history read and
  its attachment can be missing from that tab until it is reopened.
- **Daemon restart** ends live processes as before; runs are recovered and
  resumed.
- **Remote SSH:** the design is host-local (daemon, wrapper and the Claude
  extension all run on the remote host), but it has not been verified live on a
  remote host. Writing the machine-scoped setting from a remote window is
  untested.
