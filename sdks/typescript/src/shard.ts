// The client: one connection pool to `shard serve`, the sandboxes on it, and the policies, secrets and snapshots they share.
import { App } from "./app.js";
import { resolve, type ShardOptions } from "./config.js";
import { listed } from "./pages.js";
import * as records from "./records.js";
import type { Capabilities, Policy, PolicyRule, Restart, SandboxInfo, SecretInfo, Snapshot, Version } from "./records.js";
import { Sandbox, refresh } from "./sandbox.js";
import { Transport } from "./transport.js";
import * as wire from "./wire.js";

/** SandboxRef is a sandbox handle, or its id, id prefix or name. */
export type SandboxRef = Sandbox | string;

/** CreateOptions make a sandbox from an image or a snapshot, exactly one of them. */
export interface CreateOptions {
  image?: string;
  /** A snapshot id, id prefix or name; the sandbox starts from its disk and memory. */
  snapshot?: string;
  name?: string;
  /** The environment every command in the sandbox starts with. */
  env?: Record<string, string>;
  workdir?: string;
  /** A user name or uid in the sandbox, as nobody or 1000. */
  user?: string;
  /** The names of the secrets to grant. */
  secrets?: string[];
  policy?: string;
  /** The memory bound; left out, a snapshot's own bound or none. */
  memoryMiB?: number;
  /** Left out, the daemon's default. */
  vcpus?: number;
  diskMiB?: number;
}

/** RunOptions make the sandbox of a run, which only an image does. */
export interface RunOptions extends Omit<CreateOptions, "image" | "snapshot"> {
  /** When the daemon starts the app again; left out, never. */
  restart?: Restart;
}

/** SecretOptions are what a set stores: the value, and the hosts the daemon sends it to. */
export interface SecretOptions {
  value: string;
  destinations?: string[];
  /** What the sandbox holds in the value's place; the daemon picks one when left out. */
  placeholder?: string;
}

export class Shard {
  readonly policies: Policies;
  readonly secrets: Secrets;
  readonly snapshots: Snapshots;
  private readonly transport: Transport;

  /** Each option left out is read from its environment variable, as SHARD_REMOTE and SHARD_API_KEY. */
  constructor(options: ShardOptions = {}) {
    this.transport = new Transport(resolve(options));
    this.policies = new Policies(this.transport);
    this.secrets = new Secrets(this.transport);
    this.snapshots = new Snapshots(this.transport);
  }

  /** create answers a running sandbox with no app, or one whose state is failed; exec() runs in it. */
  async create(options: CreateOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.transport, await this.made(createBody(options, undefined, undefined)));
  }

  /** run answers the app command starts as in a new sandbox. The sandbox outlives the app; remove() it when done. */
  async run(image: string, command: string | string[], options: RunOptions = {}): Promise<App> {
    const { restart, ...rest } = options;
    const sandbox = new Sandbox(this.transport, await this.made(createBody({ ...rest, image }, command, restart)));

    return new App(this.transport, sandbox);
  }

  /** get answers a sandbox by id, id prefix or name. */
  async get(ref: string): Promise<Sandbox> {
    return new Sandbox(this.transport, records.sandboxInfo(await this.transport.call("GET", wire.path("sandboxes", ref))));
  }

  /** list answers the running sandboxes, or with all every sandbox the daemon holds a record of. */
  async list(options: { all?: boolean } = {}): Promise<Sandbox[]> {
    const rows = await listed(this.transport, wire.path("sandboxes"), "sandboxes", wire.query({ all: options.all }));

    return rows.map((row) => new Sandbox(this.transport, records.sandboxInfo(row)));
  }

  async version(): Promise<Version> {
    return records.version(await this.transport.call("GET", wire.path("version")));
  }

  async capabilities(): Promise<Capabilities> {
    return records.capabilities(await this.transport.call("GET", wire.path("capabilities")));
  }

  /** close lets go of the pooled connections, so the process can exit. */
  close(): void {
    this.transport.close();
  }

  private async made(body: CreateBody): Promise<SandboxInfo> {
    // wait=true answers once the sandbox is running or failed, which an image pull may take minutes to reach.
    const answer = await this.transport.call("POST", wire.path("sandboxes"), { json: body, query: { wait: "true" }, timeoutMs: 0 });

    return records.sandboxInfo(answer);
  }
}

export class Policies {
  constructor(private readonly transport: Transport) {}

  /** set makes the policy, or replaces every rule of it; a sandbox it is assigned to enforces the new rules. */
  async set(name: string, rules: PolicyRule[]): Promise<Policy> {
    const body = { rules: rules.map(({ action, rule }) => ({ action, rule })) };

    return records.policy(await this.transport.call("PUT", wire.path("policies", name), { json: body }));
  }

  async get(name: string): Promise<Policy> {
    return records.policy(await this.transport.call("GET", wire.path("policies", name)));
  }

  async list(): Promise<Policy[]> {
    return (await listed(this.transport, wire.path("policies"), "policies")).map(records.policy);
  }

  /** remove deletes a policy no sandbox holds. */
  async remove(name: string): Promise<void> {
    await this.transport.call("DELETE", wire.path("policies", name));
  }

  /** assign makes the sandbox enforce the policy from its next request on. */
  assign(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(this.transport, sandbox, "PUT", ["policy"], { policy: name });
  }

  /** clear takes the sandbox's policy away, which leaves it the daemon's default. */
  clear(sandbox: SandboxRef): Promise<SandboxInfo> {
    return changed(this.transport, sandbox, "DELETE", ["policy"]);
  }
}

export class Secrets {
  constructor(private readonly transport: Transport) {}

  /** set stores the secret, or replaces its value; the answer never holds the value. */
  async set(name: string, options: SecretOptions): Promise<SecretInfo> {
    const { value, destinations, placeholder } = options;
    const body = { value, destinations, placeholder };

    return records.secretInfo(await this.transport.call("PUT", wire.path("secrets", name), { json: body }));
  }

  async list(): Promise<SecretInfo[]> {
    return (await listed(this.transport, wire.path("secrets"), "secrets")).map(records.secretInfo);
  }

  /** remove deletes a secret no sandbox holds; force takes it from every sandbox first. */
  async remove(name: string, options: { force?: boolean } = {}): Promise<void> {
    await this.transport.call("DELETE", wire.path("secrets", name), { query: wire.query({ force: options.force }) });
  }

  /** grant lets the sandbox send the secret to its destinations; the sandbox must not be running. */
  grant(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(this.transport, sandbox, "POST", ["secrets", name]);
  }

  revoke(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(this.transport, sandbox, "DELETE", ["secrets", name]);
  }
}

export class Snapshots {
  constructor(private readonly transport: Transport) {}

  /** create takes a snapshot of a paused or stopped sandbox, which the snapshot outlives. */
  async create(sandbox: SandboxRef, options: { name?: string } = {}): Promise<Snapshot> {
    const body = { sandbox: idOf(sandbox), name: options.name || undefined };

    return records.snapshot(await this.transport.call("POST", wire.path("snapshots"), { json: body, timeoutMs: 0 }));
  }

  async list(): Promise<Snapshot[]> {
    return (await listed(this.transport, wire.path("snapshots"), "snapshots")).map(records.snapshot);
  }

  /** inspect answers a snapshot by id, id prefix or name. */
  async inspect(ref: string): Promise<Snapshot> {
    return records.snapshot(await this.transport.call("GET", wire.path("snapshots", ref)));
  }

  async remove(ref: string): Promise<void> {
    await this.transport.call("DELETE", wire.path("snapshots", ref));
  }
}

interface CreateBody {
  image?: string;
  snapshot?: string;
  name?: string;
  command?: string[];
  env?: string[];
  workdir?: string;
  user?: string;
  secrets?: string[];
  policy?: string;
  resources: { memory_mib?: number; vcpus: number; disk_mib: number };
  restart?: { policy: Restart["policy"]; retries?: number; backoff: number };
}

function createBody(options: CreateOptions, command: string | string[] | undefined, restart: Restart | undefined): CreateBody {
  const { image, snapshot, name, env, workdir, user, secrets, policy, memoryMiB, vcpus, diskMiB } = options;
  if ((image === undefined) === (snapshot === undefined)) {
    throw new TypeError("a sandbox is made from an image or a snapshot, exactly one of them");
  }
  const body: CreateBody = {
    image,
    snapshot,
    name: name || undefined,
    workdir: workdir || undefined,
    user: user || undefined,
    secrets: secrets?.length ? [...secrets] : undefined,
    policy: policy || undefined,
    resources: { memory_mib: memoryMiB, vcpus: vcpus ?? 0, disk_mib: diskMiB ?? 0 },
  };
  if (command !== undefined) {
    body.command = argv(command);
  }
  if (env && Object.keys(env).length > 0) {
    body.env = Object.entries(env).map(([key, value]) => `${key}=${value}`);
  }
  if (restart) {
    body.restart = { policy: restart.policy, retries: restart.retries || undefined, backoff: restart.backoff ?? 0 };
  }

  return body;
}

/** argv runs a string under /bin/sh -c, and an array as the argv itself. */
function argv(command: string | string[]): string[] {
  const args = typeof command === "string" ? ["/bin/sh", "-c", command] : [...command];
  if (args.length === 0) {
    throw new TypeError("command must name a program");
  }

  return args;
}

function idOf(sandbox: SandboxRef): string {
  return typeof sandbox === "string" ? sandbox : sandbox.id;
}

/** changed sends a change to a sandbox's record, and keeps a handle's info current with the answer. */
async function changed(transport: Transport, sandbox: SandboxRef, method: string, route: string[], json?: unknown): Promise<SandboxInfo> {
  const info = records.sandboxInfo(await transport.call(method, wire.path("sandboxes", idOf(sandbox), ...route), { json }));
  if (typeof sandbox !== "string") {
    sandbox[refresh](info);
  }

  return info;
}
