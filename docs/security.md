# Security Model

## What is protected

- **Prompts and the Shared Testing & Sudo Block** are encrypted with
  AES-256-GCM before touching SQLite. The master key is a random 256-bit
  value in `~/.agent-conductor/state/vault.key` (0600).
- The DB keeps only: ciphertext, sha256 hash, a redacted short preview.
- **Logs** pass through a redaction filter BEFORE write: every registered
  secret value, plus credential patterns (bearer tokens, `sk-ant-`,
  `sk-proj-`, `ghp_`), is replaced with `[REDACTED]`.
- **IPC** is a Unix domain socket (`0600`) plus a machine-local random token
  (`0600` file).
- **Webviews** use `default-src 'none'` CSP with per-load nonces, no remote
  origins, no CDN; provider/user text is rendered through React escaping.
- Daemon service units contain **no secrets**; hooks receive no secrets on
  the command line.

## What is intentionally NOT protected against

An attacker who controls your Unix account can read both the ciphertext and
the key file. This design protects against **accidental plaintext exposure**
(logs, dumps, sync tools, casual DB reads) — not a full account compromise.

## The Shared Testing & Sudo Block — informed consent

When enabled, the block is:

1. stored encrypted,
2. decrypted only at dispatch time inside the daemon,
3. appended **literally** to the provider prompt, which means it **becomes
   part of the provider conversation history** on Anthropic/OpenAI systems,
4. never written to logs, previews, titles, events, or reports,
5. versioned: queued runs attach the **latest version at dispatch**; only the
   version ID + hash are recorded for audit.

The UI shows this warning once, at first enable.

## Subscription protection

Managed Claude runs strip `ANTHROPIC_API_KEY` from the child environment
unless you explicitly enable API mode in advanced settings. This prevents
accidental API billing when your intent is subscription usage.

## Non-destructive provider hook integration

Agent Conductor merges only namespaced entries into
`~/.claude/settings.json` and `~/.codex/config.toml`:

- existing file is backed up with a timestamped copy before first change;
- unrelated hooks and settings are preserved byte-for-byte in structure;
- install is idempotent;
- uninstall removes only Agent Conductor's entries;
- unreadable provider configs are refused, never overwritten.

## Cancellation and secrets

Cancellation never deletes provider session history; worktrees are preserved
after failures. `skipRun` requires explicit confirmation in the UI.
