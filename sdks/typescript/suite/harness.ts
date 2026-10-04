import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { request } from "node:https";
import {
  NotFoundError,
  Shard,
  type App,
  type CreateOptions,
  type Policy,
  type PolicyRule,
  type RunOptions,
  type Sandbox,
  type SecretOptions,
  type Snapshot,
} from "useshards";

export const MiB = 1024 * 1024;

export interface Check {
  name: string;
  run: (ctx: Context) => Promise<void>;
}

/** Skip ends a check that cannot run here, such as a verb the provider refuses by name. */
export class Skip extends Error {}

export function skip(reason: string): never {
  throw new Skip(reason);
}

/** Run is what every check of one run shares: the client, the image, the names and the fixtures. */
interface Run {
  shard: Shard;
  image: string;
  // Both SDK suites may share one daemon, so every name carries the language and a run id.
  prefix: string;
  count: number;
  fixtures: Map<string, Promise<unknown>>;
}

/** Context is what one check sees: the run it shares, and the resources it must undo when it ends. */
export class Context {
  private cleanups: Array<{ what: string; run: () => Promise<unknown> }> = [];

  private constructor(
    private readonly shared: Run,
    // The suite's own Context owns the fixtures and undoes them after the last check.
    private readonly root: Context | undefined,
  ) {}

  static suite(): Context {
    const prefix = `suite-ts-${Math.random().toString(36).slice(2, 8)}`;
    const image = process.env.SHARD_SUITE_IMAGE || "alpine:3.20";

    return new Context({ shard: new Shard(), image, prefix, count: 0, fixtures: new Map() }, undefined);
  }

  /** check is the Context of one check, so its cleanup leaves no sandbox behind for the next one. */
  check(): Context {
    return new Context(this.shared, this.root ?? this);
  }

  get shard(): Shard {
    return this.shared.shard;
  }

  get image(): string {
    return this.shared.image;
  }

  get prefix(): string {
    return this.shared.prefix;
  }

  name(kind: string): string {
    this.shared.count++;

    return `${this.shared.prefix}-${kind}-${this.shared.count}`;
  }

  /** variable is a unique name shaped like an environment variable, which a secret name must be. */
  variable(kind: string): string {
    return this.name(kind).toUpperCase().replaceAll("-", "_");
  }

  /** client is a second, independent client, as another process would make one. */
  client(): Shard {
    return new Shard();
  }

  async create(options: Partial<CreateOptions> = {}): Promise<Sandbox> {
    const sandbox = await this.shard.create({ image: this.image, name: this.name("sandbox"), ...options });
    this.defer(`remove sandbox ${sandbox.id}`, () => sandbox.remove({ force: true }));

    return sandbox;
  }

  async run(command: string | string[], options: Partial<RunOptions> = {}): Promise<App> {
    const app = await this.shard.run(this.image, command, { name: this.name("app"), ...options });
    this.defer(`remove sandbox ${app.sandbox.id}`, () => app.sandbox.remove({ force: true }));

    return app;
  }

  /** policy sets one; cleanup runs in reverse, so a sandbox made after it is gone before the policy goes. */
  async policy(name: string, rules: PolicyRule[]): Promise<Policy> {
    const policy = await this.shard.policies.set(name, rules);
    this.defer(`remove policy ${name}`, () => this.shard.policies.remove(name));

    return policy;
  }

  async secret(name: string, options: SecretOptions): Promise<void> {
    await this.shard.secrets.set(name, options);
    this.defer(`remove secret ${name}`, () => this.shard.secrets.remove(name, { force: true }));
  }

  async snapshot(source: Sandbox): Promise<Snapshot> {
    const snapshot = await this.shard.snapshots.create(source, { name: this.name("snapshot") });
    this.defer(`remove snapshot ${snapshot.id}`, () => this.shard.snapshots.remove(snapshot.id));

    return snapshot;
  }

  /** sandbox is one running sandbox that checks which change nothing lasting share. */
  sandbox(): Promise<Sandbox> {
    return this.fixture("sandbox", (owner) => owner.create());
  }

  /** fixture makes a resource once per run, under the suite's Context, so it outlives the check that made it. */
  fixture<T>(key: string, make: (owner: Context) => Promise<T>): Promise<T> {
    const held = this.shared.fixtures.get(key);
    if (held) {
      return held as Promise<T>;
    }
    const made = make(this.root ?? this);
    this.shared.fixtures.set(key, made);

    return made;
  }

  defer(what: string, run: () => Promise<unknown>): void {
    this.cleanups.push({ what, run });
  }

  /** cleanup undoes everything in reverse and answers what failed; a resource already gone is not a failure. */
  async cleanup(): Promise<string[]> {
    const failed: string[] = [];
    for (const { what, run } of this.cleanups.splice(0).reverse()) {
      try {
        await run();
      } catch (err) {
        if (err instanceof NotFoundError) {
          continue;
        }
        failed.push(`${what}: ${describe(err)}`);
      }
    }

    return failed;
  }
}

export async function rejects<E extends Error>(
  kind: abstract new (...args: never[]) => E,
  run: () => Promise<unknown>,
): Promise<E> {
  try {
    await run();
  } catch (err) {
    if (err instanceof kind) {
      return err;
    }
    assert.fail(`want ${kind.name}, got ${describe(err)}`);
  }

  return assert.fail(`want ${kind.name}, got no error`);
}

export async function waitFor(what: string, timeoutMs: number, done: () => Promise<boolean>): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await done()) {
      return;
    }
    await sleep(200);
  }
  assert.fail(`${what} did not happen within ${timeoutMs} ms`);
}

/** consume iterates in the background and resolves with what ended it, undefined for a normal end, so it never rejects unseen. */
export async function consume<T>(source: AsyncIterable<T>, each: (item: T) => void): Promise<unknown> {
  try {
    for await (const item of source) {
      each(item);
    }
  } catch (err) {
    return err;
  }

  return undefined;
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function text(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("utf8");
}

export function describe(err: unknown): string {
  if (err instanceof Error) {
    return `${err.constructor.name}: ${err.message}`;
  }

  return String(err);
}

/** Raw is one answer the suite read without the SDK, for routes the SDK must never reach. */
export interface Raw {
  status: number;
  body: string;
}

/** raw sends one request with the suite's own key, or the one given, so a refusal is the server's and not the SDK's. */
export async function raw(method: string, path: string, key = process.env.SHARD_API_KEY ?? ""): Promise<Raw> {
  const remote = new URL(process.env.SHARD_REMOTE ?? "");
  const ca = process.env.SHARD_CA_FILE ? await readFile(process.env.SHARD_CA_FILE) : undefined;

  return new Promise((resolve, reject) => {
    const req = request(
      new URL(path, remote),
      { method, ca, headers: { Authorization: `Bearer ${key}` } },
      (res) => {
        const chunks: Buffer[] = [];
        res.on("data", (chunk: Buffer) => chunks.push(chunk));
        res.on("end", () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(chunks).toString("utf8") }));
        res.on("error", reject);
      },
    );
    req.on("error", reject);
    req.end();
  });
}
