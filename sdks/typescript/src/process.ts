// The named processes a run starts in a sandbox. Each ends on its own or by kill(), and the sandbox outlives it either way.
import { follow, logChunk } from "./follow.js";
import type { components } from "./generated/schema.js";
import { processInfo, records, type ProcessInfo, type Restart } from "./records.js";
import type { FollowOptions } from "./sandbox.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";

type RunRequest = components["schemas"]["RunRequest"];

/** RunOptions say how shard-init runs a process; env, workdir and user left out take the sandbox's own. */
export interface RunOptions {
  /** Lowercase letters, digits, '.', '_' and '-', at most 32; left out, the base name of the program. */
  name?: string;
  /** Variables added to the sandbox's environment for this process. */
  env?: Record<string, string>;
  workdir?: string;
  user?: string;
  /** When shard-init starts the process again, and whether a start of the sandbox does; left out, unless-stopped. */
  restart?: Restart;
}

/** Processes are the named processes of one sandbox, run by this client or any other. */
export class Processes {
  constructor(
    private readonly transport: Transport,
    private readonly sandboxId: string,
  ) {}

  /** start a supervised process in a running sandbox */
  async run(command: string | string[], options: RunOptions = {}): Promise<Process> {
    const params = { path: { id: this.sandboxId } };
    const { data } = await this.transport.api.POST("/v0/sandboxes/{id}/processes", { params, body: runRequest(command, options), fetch: this.transport.waiting });

    return new Process(this.transport, this.sandboxId, processInfo(data));
  }

  /** list the processes of a sandbox, in the order they were first run */
  async list(): Promise<ProcessInfo[]> {
    const params = { path: { id: this.sandboxId } };
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/processes", { params });

    return records(data?.processes, `the processes of sandbox ${this.sandboxId}`, processInfo);
  }

  /** get returns a handle to the process of that name, as the daemon last read it. */
  async get(name: string): Promise<Process> {
    const params = { path: { id: this.sandboxId, name } };
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/processes/{name}", { params });

    return new Process(this.transport, this.sandboxId, processInfo(data));
  }
}

/** Process is one named process; info is the process as the last call on this handle returned it. */
export class Process {
  private current: ProcessInfo;

  constructor(
    private readonly transport: Transport,
    readonly sandboxId: string,
    info: ProcessInfo,
  ) {
    this.current = info;
  }

  get info(): ProcessInfo {
    return this.current;
  }

  get name(): string {
    return this.current.name;
  }

  async inspect(): Promise<ProcessInfo> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/processes/{name}", { params: this.params });

    return this.keep(data);
  }

  /** wait resolves once the process ends and its restart policy starts it no more; an abort ends the wait, never the process. */
  async wait(options: FollowOptions = {}): Promise<ProcessInfo> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/processes/{name}/attach", { params: this.params, signal: options.signal, fetch: this.transport.waiting });

    return this.keep(data);
  }

  /** logs returns every run's output the host keeps, stdout and stderr as the process wrote them. */
  async logs(): Promise<string> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/processes/{name}/logs", { params: this.params, headers: { Accept: "text/plain" }, parseAs: "text" });

    return data ?? "";
  }

  /** followLogs yields the output from the start of the log, then as it arrives, and ends when the process ends or its sandbox stops. */
  followLogs(options: FollowOptions = {}): AsyncGenerator<Uint8Array> {
    const route = wire.path("sandboxes", this.sandboxId, "processes", this.name, "logs");

    return follow(this.transport, route, `the logs of process ${this.name} of sandbox ${this.sandboxId}`, logChunk, options.signal);
  }

  /** stop a process and cancel its restarts */
  async kill(options: { force?: boolean } = {}): Promise<ProcessInfo> {
    const body = options.force ? { force: true } : undefined;
    const { data } = await this.transport.api.POST("/v0/sandboxes/{id}/processes/{name}/kill", { params: this.params, body, fetch: this.transport.waiting });

    return this.keep(data);
  }

  private get params(): { path: { id: string; name: string } } {
    return { path: { id: this.sandboxId, name: this.name } };
  }

  private keep(data: unknown): ProcessInfo {
    this.current = processInfo(data);

    return this.current;
  }
}

function runRequest(command: string | string[], options: RunOptions): RunRequest {
  const { name, env, workdir, user, restart } = options;

  return {
    command: wire.argv(command),
    name: name || undefined,
    env: wire.envList(env),
    workdir: workdir || undefined,
    user: user || undefined,
    restart: restart && { policy: restart.policy, retries: restart.retries || undefined, backoff: restart.backoff || undefined },
  };
}
