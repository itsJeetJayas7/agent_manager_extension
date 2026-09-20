export declare const DAEMON_BIN_DIR: string;
export declare const DAEMON_BIN_NAME: string;
export declare const WEBVIEW_DIR: string;
export declare const WEBVIEW_BUNDLE: string;
export declare const DAEMON_STAGE_DIR: string;
export declare function daemonBinNameFor(goos: string, goarch: string): string;
export declare function goPlatform(
  nodePlatform: string,
  nodeArch: string
): { goos: string; goarch: string };
export declare const SHIPPED_PLATFORMS: { goos: string; goarch: string }[];
