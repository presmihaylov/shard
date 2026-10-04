// The client: one connection pool to `shard serve`, the sandboxes on it, and the policies, secrets and snapshots they share.
import { App } from "./app.js";
import { plainWarning, resolve, type ShardOptions } from "./config.js";
import type { components } from "./generated/schema.js";
import { listed } from "./pages.js";
import * as records from "./records.js";
import type { Capabilities, Policy, PolicyRule, Restart, SandboxInfo, SecretInfo, Snapshot, Version } from "./records.js";
import { Sandbox, refresh } from "./sandbox.js";
import { Transport } from "./transport.js";

type CreateRequest = components["schemas"]["CreateRequest"];

/** SandboxRef is a sandbox handle, or its id, id prefix or name. */
export type SandboxRef = Sandbox | string;

/** CreateOptions make a sandbox from an image or a snapshot, exactly one of them. */
export interface CreateOptions {
  image?: string;
  /** A snapshot id, id prefix or name; the sandbox starts from its files. */
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
    const settings = resolve(options);
    if (settings.baseUrl.startsWith("http:")) {
      process.emitWarning(plainWarning);
    }
    this.transport = new Transport(settings);
    this.policies = new Policies(this.transport);
    this.secrets = new Secrets(this.transport);
    this.snapshots = new Snapshots(this.transport);
  }

  /** create a sandbox */
  async create(options: CreateOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.transport, await this.made(createBody(options, undefined, undefined)));
  }

  /** create a sandbox and start its command */
  async run(image: string, command: string | string[], options: RunOptions = {}): Promise<App> {
    const { restart, ...rest } = options;
    const sandbox = new Sandbox(this.transport, await this.made(createBody({ ...rest, image }, command, restart)));

    return new App(this.transport, sandbox);
  }

  /** get answers a sandbox by id, id prefix or name. */
  async get(ref: string): Promise<Sandbox> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}", { params: { path: { id: ref } } });

    return new Sandbox(this.transport, records.sandboxInfo(data));
  }

  /** list active sandboxes */
  async list(options: { all?: boolean } = {}): Promise<Sandbox[]> {
    const all = options.all || undefined;
    const rows = await listed("/v0/sandboxes", "sandboxes", (cursor) => this.transport.api.GET("/v0/sandboxes", { params: { query: { all, cursor } } }));

    return rows.map((row) => new Sandbox(this.transport, records.sandboxInfo(row)));
  }

  async version(): Promise<Version> {
    return records.version((await this.transport.api.GET("/v0/version")).data);
  }

  async capabilities(): Promise<Capabilities> {
    return records.capabilities((await this.transport.api.GET("/v0/capabilities")).data);
  }

  /** close lets go of the pooled connections, so the process can exit. */
  close(): void {
    this.transport.close();
  }

  private async made(body: CreateRequest): Promise<SandboxInfo> {
    // wait=true answers once the sandbox is running or failed, which an image pull may take minutes to reach.
    const { data } = await this.transport.api.POST("/v0/sandboxes", { params: { query: { wait: true } }, body, fetch: this.transport.waiting });

    return records.sandboxInfo(data);
  }
}

export class Policies {
  constructor(private readonly transport: Transport) {}

  /** set makes the policy, or replaces every rule of it; a sandbox it is attached to enforces the new rules. */
  async set(name: string, rules: PolicyRule[]): Promise<Policy> {
    const body = { rules: rules.map(({ action, rule }) => ({ action, rule })) };

    const { data } = await this.transport.api.PUT("/v0/policies/{name}", { params: { path: { name } }, body });

    return records.policy(data);
  }

  async get(name: string): Promise<Policy> {
    return records.policy((await this.transport.api.GET("/v0/policies/{name}", { params: { path: { name } } })).data);
  }

  async list(): Promise<Policy[]> {
    const rows = await listed("/v0/policies", "policies", (cursor) => this.transport.api.GET("/v0/policies", { params: { query: { cursor } } }));

    return rows.map(records.policy);
  }

  /** remove deletes a policy no sandbox holds. */
  async remove(name: string): Promise<void> {
    await this.transport.api.DELETE("/v0/policies/{name}", { params: { path: { name } } });
  }

  /** attach makes the sandbox enforce the policy from its next request on; the sandbox must not be running. */
  attach(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.PUT("/v0/sandboxes/{id}/policy", { params: { path: { id } }, body: { policy: name } }));
  }

  /** detach takes the sandbox's policy away, which leaves it the daemon's default. */
  detach(sandbox: SandboxRef): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.DELETE("/v0/sandboxes/{id}/policy", { params: { path: { id } } }));
  }
}

export class Secrets {
  constructor(private readonly transport: Transport) {}

  /** set stores the secret, or replaces its value; the answer never holds the value. */
  async set(name: string, options: SecretOptions): Promise<SecretInfo> {
    const { value, destinations, placeholder } = options;
    const body = { value, destinations, placeholder };
    const { data } = await this.transport.api.PUT("/v0/secrets/{name}", { params: { path: { name } }, body });

    return records.secretInfo(data);
  }

  async list(): Promise<SecretInfo[]> {
    const rows = await listed("/v0/secrets", "secrets", (cursor) => this.transport.api.GET("/v0/secrets", { params: { query: { cursor } } }));

    return rows.map(records.secretInfo);
  }

  /** remove deletes a secret no sandbox holds; force takes it from every sandbox first. */
  async remove(name: string, options: { force?: boolean } = {}): Promise<void> {
    await this.transport.api.DELETE("/v0/secrets/{name}", { params: { path: { name }, query: { force: options.force || undefined } } });
  }

  /** grant lets the sandbox send the secret to its destinations; the sandbox must not be running. */
  grant(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.POST("/v0/sandboxes/{id}/secrets/{name}", { params: { path: { id, name } } }));
  }

  ungrant(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.DELETE("/v0/sandboxes/{id}/secrets/{name}", { params: { path: { id, name } } }));
  }
}

export class Snapshots {
  constructor(private readonly transport: Transport) {}

  /** create copies a stopped sandbox's files into a snapshot that outlives it. */
  async create(sandbox: SandboxRef, options: { name?: string } = {}): Promise<Snapshot> {
    const body = { sandbox: idOf(sandbox), name: options.name || undefined };

    const { data } = await this.transport.api.POST("/v0/snapshots", { body, fetch: this.transport.waiting });

    return records.snapshot(data);
  }

  async list(): Promise<Snapshot[]> {
    const rows = await listed("/v0/snapshots", "snapshots", (cursor) => this.transport.api.GET("/v0/snapshots", { params: { query: { cursor } } }));

    return rows.map(records.snapshot);
  }

  /** inspect answers a snapshot by id, id prefix or name. */
  async inspect(ref: string): Promise<Snapshot> {
    return records.snapshot((await this.transport.api.GET("/v0/snapshots/{ref}", { params: { path: { ref } } })).data);
  }

  async remove(ref: string): Promise<void> {
    await this.transport.api.DELETE("/v0/snapshots/{ref}", { params: { path: { ref } } });
  }
}

function createBody(options: CreateOptions, command: string | string[] | undefined, restart: Restart | undefined): CreateRequest {
  const { image, snapshot, name, env, workdir, user, secrets, policy, memoryMiB, vcpus, diskMiB } = options;
  if ((image === undefined) === (snapshot === undefined)) {
    throw new TypeError("a sandbox is made from an image or a snapshot, exactly one of them");
  }
  const body: CreateRequest = {
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
async function changed(sandbox: SandboxRef, send: (id: string) => Promise<{ data?: unknown }>): Promise<SandboxInfo> {
  const info = records.sandboxInfo((await send(idOf(sandbox))).data);
  if (typeof sandbox !== "string") {
    sandbox[refresh](info);
  }

  return info;
}
