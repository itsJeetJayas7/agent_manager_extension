// sessionOpeners routes users back to native provider conversations.
// Only documented/public surfaces are used; capability detection decides
// between exact opening and honest fallbacks.
import * as vscode from "vscode";

/** Build the Claude Code VS Code URI for a session (resume in extension). */
export function claudeSessionUri(sessionId: string): vscode.Uri {
  // Community-documented handler; verified against the installed extension
  // at runtime and degrades to the command palette when unavailable.
  return vscode.Uri.parse(`vscode://anthropic.claude-code/open?session=${encodeURIComponent(sessionId)}`);
}

export function claudeExtensionInstalled(ext: vscode.Extension<any> | undefined): boolean {
  return !!ext;
}

/** Open a managed Claude session in the official extension if possible. */
export async function openClaudeSession(sessionId: string): Promise<{ opened: boolean; how: string }> {
  const ext = vscode.extensions.getExtension("anthropic.claude-code");
  if (!ext) {
    await vscode.env.clipboard.writeText(sessionId);
    return {
      opened: false,
      how: "Claude Code extension not detected. Session ID copied — resume with `claude --resume <id>` in the project directory.",
    };
  }
  try {
    await vscode.env.openExternal(claudeSessionUri(sessionId));
    return { opened: true, how: "opened via URI handler" };
  } catch {
    await vscode.env.clipboard.writeText(sessionId);
    return {
      opened: false,
      how: "URI handler rejected. Session ID copied — use Claude Code: resume by ID.",
    };
  }
}

/**
 * Open a managed Codex thread in the official Codex experience.
 * The App Server and the IDE share CODEX_HOME, so the thread is present in
 * native history. Exact programmatic thread focusing depends on documented
 * public commands; we probe them without reverse engineering.
 */
export async function openCodexThread(threadId: string, threadTitle: string): Promise<{ opened: boolean; how: string }> {
  const ext = vscode.extensions.getExtension("openai.chatgpt") ?? vscode.extensions.getExtension("openai.codex");
  let commands: string[] = [];
  try {
    commands = await vscode.commands.getCommands(true);
  } catch {
    commands = [];
  }
  // Documented-style focus commands only; never private DOM tricks.
  const candidates = [
    "codex.extension.openSidebar",
    "openai.codex.openChat",
    "codex.openChat",
    "chatgpt.openChat",
  ];
  const available = candidates.find((c) => commands.includes(c));
  if (available) {
    try {
      await vscode.commands.executeCommand(available);
      await vscode.env.clipboard.writeText(threadTitle);
      return {
        opened: true,
        how: `Codex opened. Thread is in native history under its deterministic title (copied): ${threadTitle}`,
      };
    } catch {
      // fall through
    }
  }
  if (!ext) {
    await vscode.env.clipboard.writeText(threadTitle);
    return {
      opened: false,
      how: "Codex extension not detected. Thread title copied — find it in Codex history after installing the extension.",
    };
  }
  await vscode.env.clipboard.writeText(threadTitle);
  return {
    opened: false,
    how: "No documented thread-focus command in this Codex version. Thread title copied; select it in Codex history.",
  };
}

/** Open a run worktree in a new VS Code window. */
export async function openWorktreeWindow(worktreePath: string): Promise<void> {
  const uri = vscode.Uri.file(worktreePath);
  await vscode.commands.executeCommand("vscode.openFolder", uri, true);
}
