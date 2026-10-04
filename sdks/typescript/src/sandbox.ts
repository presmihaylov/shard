// One sandbox: its record as the last verb answered it, and the verbs, commands and files that act on it.
import { Commands, type Command, type ExecOptions, type ExecResult } from "./commands.js";
import { Files } from "./files.js";
import { follow, logChunk, networkLogEntry } from "./follow.js";
import { networkLogRecord, records, sandboxInfo, type NetworkLogRecord, type SandboxInfo } from "./records.js";
import type { SendOptions, Transport } from "./transport.js";
import * as wire from "./wire.js";

/** refresh hands a sandbox the record another verb answered, as a policy assign; the package does not export it. */
export const refresh = Symbol("refresh");

export interface FollowOptions {
  /** An abort ends the follow with the signal's reason; the sandbox runs on. */
  signal?: AbortSignal;
}

export class Sandbox {
  readonly commands: Commands;
  readonly files: Files;
  private current: SandboxInfo;

  constructor(
    private readonly transport: Transport,
    info: SandboxInfo,
  ) {
    this.current = info;
    this.commands = new Commands(transport, info.id);
    this.files = new Files(transport, info.id);
  }

  /** info is the record as the last verb on this handle answered it; inspect() reads it again. */
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
    return this.verb("GET", [], {});
  }

  /** stop ends every process in the sandbox, with TERM and then KILL after 30 seconds, and keeps its root. */
  async stop(): Promise<void> {
    await this.verb("POST", ["stop"]);
  }

  /** start boots a stopped sandbox again on the root its stop left; no app starts with it. */
  async start(): Promise<void> {
    await this.verb("POST", ["start"]);
  }

  async pause(): Promise<void> {
    await this.verb("POST", ["pause"]);
  }

  async resume(): Promise<void> {
    await this.verb("POST", ["resume"]);
  }

  /** fork answers a running copy of this sandbox, memory and all; this one runs on. */
  async fork(options: { name?: string } = {}): Promise<Sandbox> {
    const route = wire.path("sandboxes", this.id, "fork");

    return new Sandbox(this.transport, sandboxInfo(await this.transport.call("POST", route, { json: { name: options.name }, timeoutMs: 0 })));
  }

  /** remove deletes a stopped sandbox; force stops a running one first. */
  async remove(options: { force?: boolean } = {}): Promise<void> {
    await this.transport.call("DELETE", wire.path("sandboxes", this.id), { query: wire.query({ force: options.force }), timeoutMs: 0 });
  }

  /** exec runs a command and answers how it ended; with background it answers a handle once the command runs. */
  exec(command: string | string[], options: ExecOptions & { background: true }): Promise<Command>;
  exec(command: string | string[], options?: ExecOptions & { background?: false }): Promise<ExecResult>;
  exec(command: string | string[], options: ExecOptions & { background?: boolean } = {}): Promise<Command | ExecResult> {
    const { background, ...rest } = options;
    if (background) {
      return this.commands.start(command, rest);
    }

    return this.commands.run(command, rest);
  }

  /** logs answers the app's output so far, stdout and stderr as the daemon wrote them. */
  async logs(): Promise<string> {
    return (await this.transport.fetch("GET", wire.path("sandboxes", this.id, "logs"))).body.toString("utf8");
  }

  /** followLogs yields the app's output from the start of the log, then as it comes, and ends when the sandbox stops. */
  followLogs(options: FollowOptions = {}): AsyncGenerator<Uint8Array> {
    return follow(this.transport, wire.path("sandboxes", this.id, "logs"), `the logs of sandbox ${this.id}`, logChunk, options.signal);
  }

  /** networkLogs answers the egress decisions the daemon still holds, oldest first. */
  async networkLogs(): Promise<NetworkLogRecord[]> {
    const answer = await this.transport.call("GET", wire.path("sandboxes", this.id, "egress-log"));

    return records(answer, `the network log of sandbox ${this.id}`, networkLogRecord);
  }

  /** followNetworkLogs yields each egress decision as the daemon makes it, and ends when the sandbox stops. */
  followNetworkLogs(options: FollowOptions = {}): AsyncGenerator<NetworkLogRecord> {
    const what = `the network log of sandbox ${this.id}`;

    return follow(this.transport, wire.path("sandboxes", this.id, "egress-log"), what, networkLogEntry, options.signal);
  }

  /** verb sends one lifecycle call that answers the record, which this handle keeps; the daemon bounds each on its side. */
  private async verb(method: string, route: string[], options: SendOptions = { timeoutMs: 0 }): Promise<SandboxInfo> {
    return this[refresh](sandboxInfo(await this.transport.call(method, wire.path("sandboxes", this.id, ...route), options)));
  }
}
