# Overnight validation — Agent Conductor 0.2.0

Validated on 2026-10-03 on an Apple M2 laptop, macOS ARM64, Node.js 22.22.0,
Claude Code 2.1.288 and Codex 0.160.0. The local extension and persistent
LaunchAgent were installed and checked after the changes.

## Result

The extension now pins Claude Opus 5.5 explicitly (`claude-opus-5-5`), exposes
low, medium, high, xhigh and max plus the model default, and reports the model
returned by the provider on run cards. Unsupported model/effort combinations
are cleared when the model changes. Staged PLAN, BUILD and AUDIT each have
complete model/effort selectors and show the effort actually dispatched.

The Claude catalog follows the [official model configuration](https://code.claude.com/docs/en/model-config)
and [effort reference](https://platform.claude.com/docs/en/build-with-claude/effort).
Model availability still depends on the signed-in account. Aliases are
labeled as moving versions. Opus 5.5 needs Claude Code 2.1.280+; Sonnet 5.5
needs 2.1.284+. The laptop CLI was updated from 2.1.278 to 2.1.288.

Automatic recovery now goes through the normal queue, so retries cannot
bypass pauses, quota windows, row barriers or
concurrency limits. A provider's terminal event does not release its slot
until its execution has returned and its child has been reaped.

## Evidence

| Check | Result |
| --- | --- |
| Go tests with `-race -count=1` | 178 test/subtest results passed across 11 packages; two additional packages have no tests |
| Extension and webview Vitest suites | 82 tests passed in nine files |
| TypeScript checks, Go vet, gofmt, diff whitespace checks | Passed |
| Real host HTML + production bundle boot | Passed, including CSP and VS Code bridge handshake |
| Chrome UI interactions | Passed at sidebar widths 280, 320, 420 and 800 px; no horizontal overflow; explicit model/effort RPC, staged selectors and incompatible effort reset checked |
| Real daemon/IPC integration | Passed authentication rejection, lifecycle, redaction, encrypted secrets, completion gate, timers, ordering, cancellation, deletion and restart persistence |
| Live Claude and Codex integration | Passed real file edits in an isolated Git repo, serial dispatch, exact `claude-opus-5-5` provider report at xhigh and a further edit in the same Claude session |
| Dependency audit | Zero reported vulnerabilities, including development dependencies; production-only audit also clean |
| Release build | macOS ARM64/x64 and Linux ARM64/x64 binaries built; Linux binaries confirmed statically linked ELF |
| VSIX inspection | Version 0.2.0; all four platform binaries match build output; 15 archive entries; local Kilo worktrees/configuration excluded |
| Local reinstall | VS Code CLI lists `agent-conductor-local.agent-conductor@0.2.0`; installed bundles/binary match the VSIX |
| Background service | Actual LaunchAgent installation and automatic restart verified; daemon health, model ladder and recovery settings checked over authenticated IPC |

The live test was run twice during this work. All five Opus effort values
are tested in generated launch/resume arguments; the paid live tests exercised
xhigh. The browser test uses the production bundle with a simulated VS Code
message bridge in a fresh Chrome profile. It is not a full VS Code UI automation
test. The installed extension files and running daemon were checked separately.

## Measured load

`node tests/integration/daemon-stress.mjs` used a real daemon, Unix socket,
SQLite state and the free fake provider in an isolated `AC_ROOT`:

| Measurement | Observed |
| --- | ---: |
| Queued jobs, all completed | 100 |
| Enqueue time for all jobs | 167 ms |
| Completion time after resuming | 5.61 seconds |
| Peak simultaneous managed runs | 1 |
| Snapshot requests | 500, in bursts of 25 outstanding requests over one connection |
| Snapshot latency, p50 / p95 / max | 11.25 / 21.04 / 26.82 ms |
| Daemon resident memory, before / after | 41.9 / 42.8 MiB |
| Warm capability-cache benchmark | 29.88 ns/op, zero allocations |

A separate regression test starts 100 simultaneous capability callers and
verifies that each cold/stale burst starts only one provider probe. These
are short local measurements, not a prediction of model response times or a
long-duration memory-leak guarantee.

## Failure handling

| Situation | Automatic behavior | When action is needed |
| --- | --- | --- |
| Network interruption, provider overload, process crash | Up to eight retries by default, with delays doubling from 5 seconds to a maximum of 5 minutes; preserve native session and encrypted recovery prompt | Exhausting the configured retry budget leaves a visible failure |
| Quota exhausted with reset time | Wait for every exhausted window to reset, then queue the same-session continuation | None unless the provider remains unavailable |
| Quota reset unknown | Wait for reliable data; a user-supplied weekly deadline can fill an unknown weekly reset and is persisted | Provide the weekly reset if known; an unknown five-hour reset is not guessed |
| Quota warning while usage is still allowed | Continue running | None |
| Agent ends without COMPLETE/BLOCKED | Continue up to three times by default; applies to missing PLAN_READY too | Then NEEDS_ATTENTION; inspect the task/session |
| Explicit BLOCKED result | Stop and retain the reason, including during PLAN | Resolve the stated blocker, then Continue |
| Authentication, billing, permission, context limit or unavailable model | Surface the classified failure without repeated automatic retries | Fix the underlying issue; Continue or Retry as appropriate |
| Verification fails | Return bounded, redacted output to the same agent and reverify, up to the configured cycle limit | Repeated failure becomes NEEDS_ATTENTION |
| Verification hangs or is cancelled | Stop the command process group and cap captured output; cancelling never launches a repair turn | Inspect the failed command if necessary |
| Pause overlaps a quota reset or retry | Keep the work queued; dispatch only when all gates allow it | Resume the queue or let its timer expire |
| Cancelled queued/native waiting run | Cancel immediately without holding a slot | None |
| Provider reports done but process is still alive | Hold the slot until the execution returns; cancellation/deadline cleanup reaps children | Persistent failures follow the bounded retry policy |
| Daemon restart with a known native session | Recover through the queue using persisted state | A run without a recoverable session requires attention |
| External Claude session emits tool/notification events | Preserve busy status until an idle event or the existing stale-session timeout | External Codex hook support remains unverified |
| Parallel worktree conflict or failed row | Hold the dependent row for explicit resolution/retry/skip | Resolve the conflict or failed prerequisite |
| Read-only AUDIT finds an issue | Report BLOCKED with the issue; audit counters are persisted and bounded | Resolve it and continue; auditing does not silently gain write permission |

Retries and marker continuations are configurable in CONTROL → Scheduling &
limits. Zero disables the corresponding automatic recovery. Retrying transient
failures is bounded intentionally: an external dependency, missing permission,
exhausted quota or ambiguous task cannot be guaranteed to finish unattended.

## Before the first VPS overnight run

1. Install `agent-conductor-0.2.0.vsix` into the Remote SSH extension host.
   Both Linux architectures are included. The sidebar should identify the
   remote host; install/login to Claude Code on that host too.
2. Run `claude --version`, update it if required, and approve the intended
   working folder with Agent Conductor's Trust Workspace action.
3. Select **Claude Opus 5.5**, the desired effort and the intended permission
   mode. Staged Intelligence overrides the composer: the defaults are Opus
   5.5 for PLAN/AUDIT and Sonnet 5.5 for BUILD. Set every staged model to Opus
   5.5 if that is the desired configuration.
4. Install the background service. Verify `systemctl --user is-active
   agent-conductor.service` and `~/.local/bin/agent-conductor diagnostics`.
   Enable lingering with `loginctl enable-linger "$USER"` if needed so it
   survives SSH logout. The service installer now reports systemctl failures
   and restarts the unit when upgrading.
5. Configure the repository's verification commands and reasonable recovery
   limits. Run one small file-edit task, verify its actual model/result,
   disconnect SSH, reconnect and verify the daemon and queue state.
6. Queue the real work, check that manual pause/quiet hours/external-session
   holds reflect your intent, and inspect any existing attention states before
   leaving it unattended. Window Anchor is optional and is not required to
   continue queued work.

See [remote-vps.md](remote-vps.md) for service paths and troubleshooting.

## Local installation and limits

The local SQLite database was backed up before migration, and the old idle
0.1.0 daemon was stopped cleanly. The new LaunchAgent runs the versioned binary
under `~/.local/share/agent-conductor/bin/0.2.0/`; provider awareness hooks point
to the stable `~/.local/bin/agent-conductor` launcher. Existing provider
configuration was backed up by the hook installer. The service restarted
successfully during an intentional idle-daemon shutdown.

Use **Developer: Reload Window** in the currently open VS Code window to
load the newly installed extension UI. Keep the laptop awake for local jobs;
no daemon can execute while the machine is asleep or powered off.

This session did **not** include an actual eight-hour soak, VPS access, Linux
runtime execution, a real provider quota exhaustion or injected live billing/
authentication failures. Those recovery paths are covered with controlled
adapters, virtual time and process tests. Docker was installed but its engine
was unavailable, so Linux validation here stops at compilation, binary format
inspection and service-generation/error-handling tests. Native provider
behavior and external Codex hook support remain integration boundaries.

## Reproduce

```bash
npm ci
npm run lint
npm test
(cd daemon && go vet ./...)
npm run test:boot
npm run test:e2e
npm run test:stress
npm run test:browser     # installed Chrome; CHROME_PATH can override it
npm run test:live        # signed-in real providers; consumes real quota
npm run package
```

`npm run ci` includes the free daemon load check. Browser and live provider
tests remain opt-in because they need a browser or signed-in provider runtimes.
