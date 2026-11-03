// One sandbox: its record as the last verb answered it, and the verbs, commands, processes, files and ports that act on it.
import { Commands, type Command, type ExecOptions, type ExecResult } from "./commands.js";
import { Files } from "./files.js";
import { egressLogEntry, follow } from "./follow.js";
import { Ports } from "./ports.js";
import { Processes, type Process, type RunOptions } from "./process.js";
import { egressDecision, records, sandboxInfo, type EgressDecision, type SandboxInfo } from "./records.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";

/** refresh hands a sandbox the record another verb answered, as a policy attach; the package does not export it. */
export const refresh = Symbol("refresh");

export interface FollowOptions {
  /** An abort ends the follow with the signal's reason; what it follows runs on. */
  signal?: AbortSignal;
}

/** Sandbox is one sandbox; info is the sandbox as the last call on this handle returned it. */
export class Sandbox {
  readonly commands: Commands;
  readonly processes: Processes;
  readonly files: Files;
  readonly ports: Ports;
  private current: SandboxInfo;

  constructor(
    private readonly transport: Transport,
    info: SandboxInfo,
  ) {
    this.current = info;
    this.commands = new Commands(transport, info.id);
    this.processes = new Processes(transport, info.id);
    this.files = new Files(transport, info.id);
    this.ports = new Ports(transport, info.id);
  }

  /** info is the sandbox as the last call on this handle returned it; inspect() reads it again. */
  get info(): SandboxInfo {
    return this.current;
  }

  get id(): string {
    return this.current.id;
  }

  get name(): string | null {
    return this.current.name;
  }

  [refresh](info: SandboxInfo): SandboxInfo {
    this.current = info;

    return info;
  }

  inspect(): Promise<SandboxInfo> {
    return this.verb(this.transport.api.GET("/v0/sandboxes/{id}", { params: this.params }));
  }

  /** stop a sandbox and preserve its files */
  async stop(): Promise<void> {
    await this.verb(this.transport.api.POST("/v0/sandboxes/{id}/stop", { params: this.params, fetch: this.transport.waiting }));
  }

  /** start a stopped sandbox with its saved files */
  async start(): Promise<void> {
    await this.verb(this.transport.api.POST("/v0/sandboxes/{id}/start", { params: this.params, fetch: this.transport.waiting }));
  }

  /** save a sandbox's state and suspend it */
  async pause(): Promise<void> {
    await this.verb(this.transport.api.POST("/v0/sandboxes/{id}/pause", { params: this.params, fetch: this.transport.waiting }));
  }

  /** resume a paused sandbox from its saved state */
  async resume(): Promise<void> {
    await this.verb(this.transport.api.POST("/v0/sandboxes/{id}/resume", { params: this.params, fetch: this.transport.waiting }));
  }

  /** create a sandbox from a running sandbox's memory and files */
  async fork(options: { name?: string } = {}): Promise<Sandbox> {
    const body = { name: options.name };
    const { data } = await this.transport.api.POST("/v0/sandboxes/{id}/fork", { params: this.params, body, fetch: this.transport.waiting });

    return new Sandbox(this.transport, sandboxInfo(data));
  }

  /** remove a sandbox and its files */
  async remove(options: { force?: boolean } = {}): Promise<void> {
    const params = { ...this.params, query: { force: options.force || undefined } };
    await this.transport.api.DELETE("/v0/sandboxes/{id}", { params, fetch: this.transport.waiting });
  }

  /** execute a command in a running sandbox */
  exec(command: string | string[], options: ExecOptions & { background: true }): Promise<Command>;
  exec(command: string | string[], options?: ExecOptions & { background?: false }): Promise<ExecResult>;
  exec(command: string | string[], options: ExecOptions & { background?: boolean } = {}): Promise<Command | ExecResult> {
    const { background, ...rest } = options;
    if (background) {
      return this.commands.start(command, rest);
    }

    return this.commands.run(command, rest);
  }

  /** start a supervised process in a running sandbox */
  run(command: string | string[], options: RunOptions = {}): Promise<Process> {
    return this.processes.run(command, options);
  }

  /** egressLog returns the egress decisions the daemon still holds, oldest first. */
  async egressLog(): Promise<EgressDecision[]> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/egress-log", { params: this.params });

    return records(data, `the egress log of sandbox ${this.id}`, egressDecision);
  }

  /** followEgressLog yields each egress decision as the daemon makes it, and ends when the sandbox stops. */
  followEgressLog(options: FollowOptions = {}): AsyncGenerator<EgressDecision> {
    const what = `the egress log of sandbox ${this.id}`;

    return follow(this.transport, wire.path("sandboxes", this.id, "egress-log"), what, egressLogEntry, options.signal);
  }

  private get params(): { path: { id: string } } {
    return { path: { id: this.id } };
  }

  /** verb keeps the record a lifecycle call answers; the daemon bounds each call that waits on the guest on its side. */
  private async verb(call: Promise<{ data?: unknown }>): Promise<SandboxInfo> {
    return this[refresh](sandboxInfo((await call).data));
  }
}
