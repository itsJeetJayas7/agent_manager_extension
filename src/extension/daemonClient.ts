// DaemonClient connects the extension host to the conductor daemon over its
// Unix domain socket (NDJSON, protocol version 1, token auth).
import * as net from "net";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";

export const PROTOCOL_VERSION = 1;

/** Interactive calls should fail fast; the UI has to stay responsive. */
export const DEFAULT_REQUEST_TIMEOUT_MS = 10_000;

export interface DaemonEvent {
  event: string;
  payload?: unknown;
}

/** Thrown when daemon and extension disagree on the wire protocol. */
export class ProtocolMismatchError extends Error {
  constructor(readonly daemonProtocol: number) {
    super(
      `Agent Conductor daemon speaks protocol ${daemonProtocol}, this extension speaks ${PROTOCOL_VERSION}. ` +
        (daemonProtocol > PROTOCOL_VERSION
          ? "Update the extension."
          : "Update the daemon (Agent Conductor: Install Background Service).")
    );
    this.name = "ProtocolMismatchError";
  }
}

type Pending = { resolve: (v: any) => void; reject: (e: Error) => void; timer: NodeJS.Timeout };

export class DaemonClient {
  private sockPath: string;
  private token: string;
  private conn: net.Socket | null = null;
  private buffer = "";
  private nextId = 1;
  private pending = new Map<string, Pending>();
  private eventHandler: ((ev: DaemonEvent) => void) | null = null;
  private closedHandler: (() => void) | null = null;
  /** Guards against `onDead` firing twice (socket emits both error and close). */
  private dead = false;
  private subscribed = false;
  connected = false;
  daemonVersion = "";

  constructor(sockPath?: string) {
    const root = path.join(os.homedir(), ".agent-conductor");
    this.sockPath = sockPath ?? path.join(root, "run", "conductor.sock");
    this.token = "";
  }

  static socketPath(): string {
    return path.join(os.homedir(), ".agent-conductor", "run", "conductor.sock");
  }

  static tokenPath(): string {
    return path.join(os.homedir(), ".agent-conductor", "run", "conductor.token");
  }

  async connect(timeoutMs = 4000): Promise<void> {
    const token = fs.readFileSync(DaemonClient.tokenPath(), "utf8").trim();
    this.token = token;
    this.dead = false;
    this.subscribed = false;
    await new Promise<void>((resolve, reject) => {
      const sock = net.createConnection({ path: this.sockPath });
      const timer = setTimeout(() => {
        sock.destroy();
        reject(new Error(`daemon connect timeout at ${this.sockPath}`));
      }, timeoutMs);
      sock.on("connect", () => {
        clearTimeout(timer);
        this.conn = sock;
        this.connected = true;
        this.wire(sock);
        this.request<{ protocol?: number; version?: string }>("hello", { auth: this.token })
          .then((hello) => {
            const proto = Number(hello?.protocol ?? 0);
            if (proto !== PROTOCOL_VERSION) {
              const err = new ProtocolMismatchError(proto);
              this.close();
              reject(err);
              return;
            }
            this.daemonVersion = String(hello?.version ?? "");
            resolve();
          })
          .catch(reject);
      });
      sock.on("error", (e) => {
        clearTimeout(timer);
        if (!this.connected) {
          reject(e);
        }
      });
    });
  }

  private wire(sock: net.Socket) {
    sock.on("data", (chunk) => {
      this.buffer += chunk.toString("utf8");
      let idx: number;
      while ((idx = this.buffer.indexOf("\n")) >= 0) {
        const line = this.buffer.slice(0, idx);
        this.buffer = this.buffer.slice(idx + 1);
        if (!line.trim()) {
          continue;
        }
        this.handleLine(line);
      }
    });
    const onDead = () => {
      if (this.dead) {
        return;
      }
      this.dead = true;
      this.connected = false;
      this.conn = null;
      for (const [, p] of this.pending) {
        clearTimeout(p.timer);
        p.reject(new Error("daemon connection closed"));
      }
      this.pending.clear();
      this.closedHandler?.();
    };
    sock.on("close", onDead);
    sock.on("error", onDead);
  }

  private handleLine(line: string) {
    let msg: any;
    try {
      msg = JSON.parse(line);
    } catch {
      return;
    }
    if (msg.event) {
      this.eventHandler?.(msg as DaemonEvent);
      return;
    }
    if (msg.id !== undefined) {
      const p = this.pending.get(String(msg.id));
      if (p) {
        this.pending.delete(String(msg.id));
        clearTimeout(p.timer);
        if (msg.error) {
          p.reject(new Error(`${msg.error.code}: ${msg.error.message}`));
        } else {
          p.resolve(msg.result);
        }
      }
    }
  }

  request<T = any>(method: string, params?: any, timeoutMs = DEFAULT_REQUEST_TIMEOUT_MS): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      if (!this.conn) {
        reject(new Error("daemon not connected"));
        return;
      }
      const id = String(this.nextId++);
      const timer = setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error(`daemon request timeout: ${method}`));
        }
      }, timeoutMs);
      // Never keep the extension host alive just for an in-flight request.
      timer.unref?.();
      this.pending.set(id, { resolve, reject, timer });
      const msg = { protocol: PROTOCOL_VERSION, id, method, params: params ?? {} };
      this.conn.write(JSON.stringify(msg) + "\n");
    });
  }

  subscribe(handler: (ev: DaemonEvent) => void): void {
    this.eventHandler = handler;
    // The daemon leaks a fan-out goroutine per subscribe; one per connection.
    if (this.subscribed) {
      return;
    }
    this.subscribed = true;
    this.request("subscribe", {}).catch(() => {
      this.subscribed = false;
    });
  }

  onClose(handler: () => void): void {
    this.closedHandler = handler;
  }

  close(): void {
    this.dead = true;
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
    }
    this.pending.clear();
    this.conn?.end();
    this.conn?.destroy();
    this.conn = null;
    this.connected = false;
  }
}
