// Minimal `vscode` module stub so extension-host modules can be unit-tested
// outside an Extension Development Host. Only the surface the tested modules
// actually touch is implemented; anything else throws loudly rather than
// silently returning undefined.
export const __config: Record<string, unknown> = {};

export const workspace = {
  getConfiguration(section: string) {
    return {
      get<T>(key: string): T | undefined {
        return __config[`${section}.${key}`] as T | undefined;
      },
    };
  },
  workspaceFolders: undefined as undefined | { uri: { fsPath: string } }[],
};

export const env = { remoteName: undefined as string | undefined };

export class ThemeColor {
  constructor(readonly id: string) {}
}

export const Uri = {
  joinPath(base: { fsPath: string }, ...parts: string[]) {
    const fsPath = [base.fsPath, ...parts].join("/");
    return { fsPath, toString: () => `file://${fsPath}` };
  },
  file(p: string) {
    return { fsPath: p, toString: () => `file://${p}` };
  },
};

export const window = {
  showInformationMessage: async () => undefined,
  showWarningMessage: async () => undefined,
  showErrorMessage: async () => undefined,
};

export const commands = {
  executeCommand: async (_command: string) => undefined,
};

export function __reset(): void {
  for (const k of Object.keys(__config)) {
    delete __config[k];
  }
  env.remoteName = undefined;
}
