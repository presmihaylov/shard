// The one app a run starts in its sandbox. It ends on its own or by stop(), and the sandbox outlives it either way.
import { ProtocolError } from "./errors.js";
import { appExit, type AppExit, type AppInfo } from "./records.js";
import type { Sandbox } from "./sandbox.js";
import type { Transport } from "./transport.js";

/** App is the app a run started. It ends on its own or by stop(); the sandbox outlives it either way. */
export class App {
  constructor(
    private readonly transport: Transport,
    readonly sandbox: Sandbox,
  ) {}

  async inspect(): Promise<AppInfo> {
    const info = await this.sandbox.inspect();
    if (info.app === null) {
      throw new ProtocolError(`the daemon answered sandbox ${info.id} with no app`);
    }

    return info.app;
  }

  /** wait returns how the app ended once its restart policy starts it no more; an abort ends the wait, never the app. */
  async wait(options: { signal?: AbortSignal } = {}): Promise<AppExit> {
    const params = { path: { id: this.sandbox.id } };
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/attach", { params, signal: options.signal, fetch: this.transport.waiting });

    return appExit(data);
  }

  /** logs returns the app's output so far; the daemon keeps a bounded log, so a long run may hold only its end. */
  logs(): Promise<string> {
    return this.sandbox.logs();
  }

  /** stop ends the app with TERM, or KILL with force, and cancels its restart policy; the sandbox keeps running. */
  async stop(options: { force?: boolean } = {}): Promise<void> {
    const params = { path: { id: this.sandbox.id } };
    const body = options.force ? { force: true } : undefined;
    await this.transport.api.POST("/v0/sandboxes/{id}/app/stop", { params, body, fetch: this.transport.waiting });
  }
}
