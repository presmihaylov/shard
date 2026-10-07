// The client: one connection pool to `shard serve`, the sandboxes on it, and the policies, secrets and snapshots they share.
import { plainWarning, resolve, type ShardOptions } from "./config.js";
import type { components } from "./generated/schema.js";
import { listed } from "./pages.js";
import * as records from "./records.js";
import type { Capabilities, Policy, PolicyRule, Port, SandboxInfo, SecretInfo, Snapshot, Version } from "./records.js";
import { Sandbox, refresh } from "./sandbox.js";
import { Transport } from "./transport.js";
import * as wire from "./wire.js";

type CreateRequest = components["schemas"]["CreateRequest"];

/** SandboxRef is a sandbox handle, or its id, id prefix or name. */
export type SandboxRef = Sandbox | string;

export interface SandboxList {
  sandboxes: Sandbox[];
  warnings: string[];
}

export interface SecretList {
  secrets: SecretInfo[];
  warnings: string[];
}

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
  /** The memory bound; left out, a snapshot's own bound, or else 512 MiB on firecracker and vz and none on gvisor, runc and sysbox. */
  memoryMiB?: number;
  /** Left out, the daemon's default. */
  vcpus?: number;
  diskMiB?: number;
  /** The host ports to forward from the sandbox's first start; public is false when left out. */
  ports?: PortOptions[];
  /** The swap file on the disk, which diskMiB counts; left out, 2048 MiB on firecracker and vz and none on gvisor, runc and sysbox, which refuse any but 0. */
  swapMiB?: number;
}

/** PortOptions are one forward a create asks for: hostPort carries to guestPort on the sandbox's 127.0.0.1, and public listens on every interface of the host. */
export interface PortOptions {
  hostPort: number;
  guestPort: number;
  public?: boolean;
}

/** SecretOptions are what a set stores: the value, and the hosts the daemon sends it to. */
export interface SecretOptions {
  value: string;
  destinations?: string[];
  /** What the sandbox holds in the value's place; the daemon picks one when left out. */
  placeholder?: string;
}

/** Shard is a client of one shard daemon; remote and apiKey default to SHARD_REMOTE and SHARD_API_KEY. */
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
    return new Sandbox(this.transport, await this.made(createBody(options)));
  }

  /** get returns a sandbox by id, id prefix or name. */
  async get(ref: string): Promise<Sandbox> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}", { params: { path: { id: ref } } });

    return new Sandbox(this.transport, records.sandboxInfo(data));
  }

  /** list active sandboxes */
  async list(options: { all?: boolean } = {}): Promise<SandboxList> {
    const all = options.all || undefined;
    const { rows, warnings } = await listed("/v0/sandboxes", "sandboxes", (cursor) => this.transport.api.GET("/v0/sandboxes", { params: { query: { all, cursor } } }));

    return { sandboxes: rows.map((row) => new Sandbox(this.transport, records.sandboxInfo(row))), warnings };
  }

  async version(): Promise<Version> {
    return records.version((await this.transport.api.GET("/v0/version")).data);
  }

  /** ports lists the forwards of every sandbox, in host port order. */
  async ports(): Promise<Port[]> {
    const { rows } = await listed("/v0/ports", "ports", (cursor) => this.transport.api.GET("/v0/ports", { params: { query: { cursor } } }));

    return rows.map(records.port);
  }

  /** capabilities returns which of the nine lifecycle verbs the daemon's provider supports, and whether it gives swap. */
  async capabilities(): Promise<Capabilities> {
    return records.capabilities((await this.transport.api.GET("/v0/capabilities")).data);
  }

  /** close closes the pooled connections, so the process can exit. */
  close(): void {
    this.transport.close();
  }

  private async made(body: CreateRequest): Promise<SandboxInfo> {
    // wait=true answers once the sandbox is running or failed, which an image pull may take minutes to reach.
    const { data } = await this.transport.api.POST("/v0/sandboxes", { params: { query: { wait: true } }, body, fetch: this.transport.waiting });

    return records.sandboxInfo(data);
  }
}

/** Policies are the named egress policies, and which sandbox enforces which. */
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
    const { rows } = await listed("/v0/policies", "policies", (cursor) => this.transport.api.GET("/v0/policies", { params: { query: { cursor } } }));

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

/** Secrets are the secrets by name; the SDK sends a value and never reads one back. */
export class Secrets {
  constructor(private readonly transport: Transport) {}

  /** set stores the secret, or replaces its value. A sandbox sees only the placeholder, and the proxy puts the value in; the SDK never reads the value back. */
  async set(name: string, options: SecretOptions): Promise<SecretInfo> {
    const { value, destinations, placeholder } = options;
    const body = { value, destinations, placeholder };
    const { data } = await this.transport.api.PUT("/v0/secrets/{name}", { params: { path: { name } }, body });

    return records.secretInfo(data);
  }

  async list(): Promise<SecretList> {
    const { rows, warnings } = await listed("/v0/secrets", "secrets", (cursor) => this.transport.api.GET("/v0/secrets", { params: { query: { cursor } } }));

    return { secrets: rows.map(records.secretInfo), warnings };
  }

  /** remove deletes a secret no sandbox holds; force deletes it anyway, and a grant left redeems nothing. */
  async remove(name: string, options: { force?: boolean } = {}): Promise<void> {
    await this.transport.api.DELETE("/v0/secrets/{name}", { params: { path: { name }, query: { force: options.force || undefined } } });
  }

  /** grant lets the sandbox send the secret to its destinations; the sandbox must not be running. */
  grant(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.POST("/v0/sandboxes/{id}/secrets/{name}", { params: { path: { id, name } } }));
  }

  /** ungrant takes the secret from the sandbox; the sandbox must not be running. */
  ungrant(sandbox: SandboxRef, name: string): Promise<SandboxInfo> {
    return changed(sandbox, (id) => this.transport.api.DELETE("/v0/sandboxes/{id}/secrets/{name}", { params: { path: { id, name } } }));
  }
}

/** Snapshots are the snapshots of stopped sandboxes, which create({ snapshot }) makes new sandboxes from. */
export class Snapshots {
  constructor(private readonly transport: Transport) {}

  /** create copies a stopped sandbox's files into a snapshot that outlives it. */
  async create(sandbox: SandboxRef, options: { name?: string } = {}): Promise<Snapshot> {
    const body = { sandbox: idOf(sandbox), name: options.name || undefined };

    const { data } = await this.transport.api.POST("/v0/snapshots", { body, fetch: this.transport.waiting });

    return records.snapshot(data);
  }

  async list(): Promise<Snapshot[]> {
    const { rows } = await listed("/v0/snapshots", "snapshots", (cursor) => this.transport.api.GET("/v0/snapshots", { params: { query: { cursor } } }));

    return rows.map(records.snapshot);
  }

  /** inspect returns a snapshot by id, id prefix or name. */
  async inspect(ref: string): Promise<Snapshot> {
    return records.snapshot((await this.transport.api.GET("/v0/snapshots/{ref}", { params: { path: { ref } } })).data);
  }

  async remove(ref: string): Promise<void> {
    await this.transport.api.DELETE("/v0/snapshots/{ref}", { params: { path: { ref } } });
  }
}

function createBody(options: CreateOptions): CreateRequest {
  const { image, snapshot, name, env, workdir, user, secrets, policy, memoryMiB, vcpus, diskMiB, ports, swapMiB } = options;
  if ((image === undefined) === (snapshot === undefined)) {
    throw new TypeError("a sandbox is made from an image or a snapshot, exactly one of them");
  }

  return {
    image,
    snapshot,
    name: name || undefined,
    env: wire.envList(env),
    workdir: workdir || undefined,
    user: user || undefined,
    secrets: secrets?.length ? [...secrets] : undefined,
    policy: policy || undefined,
    resources: { memory_mib: memoryMiB, vcpus: vcpus ?? 0, disk_mib: diskMiB ?? 0, swap_mib: swapMiB },
    ports: ports?.length ? ports.map((each) => ({ host_port: each.hostPort, guest_port: each.guestPort, public: each.public || undefined })) : undefined,
  };
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
