import assert from "node:assert/strict";
import { ConflictError, NotFoundError, UnsupportedError, type Sandbox } from "useshards";
import { rejects, skip, type Check } from "../harness.js";

/** optional runs a verb a provider may refuse by name, and skips the check when it does. */
async function optional<T>(verb: string, run: () => Promise<T>): Promise<T> {
  try {
    return await run();
  } catch (err) {
    if (err instanceof UnsupportedError) {
      skip(`the provider has no ${verb}: ${err.message}`);
    }
    throw err;
  }
}

/** others lists the guest processes besides init and the ps that lists them. */
async function others(sandbox: Sandbox): Promise<string[]> {
  const result = await sandbox.exec(["ps", "-o", "args"]);
  assert.equal(result.exitCode, 0, result.stderr);

  // Kernel threads print in brackets on a microVM, and a container has none.
  return result.stdout
    .split("\n")
    .slice(1)
    .filter((line) => line.length > 0 && !line.startsWith("[") && line !== "ps -o args")
    .filter((line) => !/^(\S*\/)?(shard-)?init\b/.test(line));
}

/** agrees runs a verb and proves the capabilities said whether the server would refuse it. */
async function agrees(verb: string, supported: boolean, run: () => Promise<unknown>): Promise<void> {
  try {
    await run();
  } catch (err) {
    if (!(err instanceof UnsupportedError)) {
      throw err;
    }
    assert.equal(supported, false, `capabilities say ${verb}, but the server refused it: ${err.message}`);
    assert.match(err.message, new RegExp(verb), `the refusal names ${verb}`);
    return;
  }
  assert.equal(supported, true, `capabilities say no ${verb}, but it ran`);
}

export const checks: Check[] = [
  {
    name: "lifecycle.create_no_app",
    run: async (ctx) => {
      const sandbox = await ctx.create({ env: { MODE: "test" }, memoryMiB: 256 });
      assert.equal(sandbox.info.state, "running");
      assert.equal(sandbox.info.resources.memoryMiB, 256);
      assert.equal(sandbox.info.app, null, "create starts no app");
      assert.equal((await sandbox.exec("printenv MODE")).stdout, "test\n");
      assert.deepEqual(await others(sandbox), [], "only init and the exec itself run in a sandbox with no app");
    },
  },
  {
    name: "lifecycle.inspect_get_list",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const info = await sandbox.inspect();
      assert.equal(info.id, sandbox.id);
      assert.equal(info.state, "running");
      assert.ok(info.image.endsWith(ctx.image), `image ${info.image}`);
      assert.equal((await ctx.shard.get(sandbox.id)).id, sandbox.id);
      assert.ok(sandbox.name, "the harness names every sandbox");
      assert.equal((await ctx.shard.get(sandbox.name)).id, sandbox.id);
      const listed = await ctx.shard.list();
      assert.ok(listed.sandboxes.some((each) => each.id === sandbox.id), "list holds the sandbox");
      await rejects(NotFoundError, () => ctx.shard.get(`${ctx.prefix}-no-such-sandbox`));
    },
  },
  {
    name: "lifecycle.stop_start",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      await sandbox.exec("echo kept > /root/kept");
      await sandbox.stop();
      assert.equal((await sandbox.inspect()).state, "stopped");
      await rejects(ConflictError, () => sandbox.exec("true"));
      await sandbox.start();
      assert.equal((await sandbox.inspect()).state, "running");
      assert.equal((await sandbox.exec("cat /root/kept")).stdout, "kept\n", "a start keeps the root a stop left");
    },
  },
  {
    name: "lifecycle.pause_resume",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      await sandbox.exec("echo before > /root/before");
      await optional("pause", () => sandbox.pause());
      assert.equal((await sandbox.inspect()).state, "paused");
      await rejects(ConflictError, () => sandbox.exec("true"));
      await sandbox.resume();
      assert.equal((await sandbox.inspect()).state, "running");
      assert.equal((await sandbox.exec("cat /root/before")).stdout, "before\n");
    },
  },
  {
    name: "lifecycle.fork",
    run: async (ctx) => {
      const source = await ctx.create();
      await source.exec("echo source > /root/origin");
      const fork = await optional("fork", () => source.fork({ name: ctx.name("fork") }));
      ctx.defer(`remove sandbox ${fork.id}`, () => fork.remove({ force: true }));
      assert.notEqual(fork.id, source.id);
      assert.equal((await fork.inspect()).state, "running");
      assert.equal((await fork.exec("cat /root/origin")).stdout, "source\n", "the fork holds the source's files");
      await fork.exec("echo fork > /root/origin");
      assert.equal((await source.exec("cat /root/origin")).stdout, "source\n", "a write in the fork leaves the source");
      assert.equal((await source.inspect()).state, "running", "the source runs on");
    },
  },
  {
    name: "lifecycle.remove",
    run: async (ctx) => {
      const running = await ctx.create();
      await rejects(ConflictError, () => running.remove());
      await running.remove({ force: true });
      await rejects(NotFoundError, () => ctx.shard.get(running.id));
      const stopped = await ctx.create();
      await stopped.stop();
      await stopped.remove();
      await rejects(NotFoundError, () => stopped.inspect());
    },
  },
  {
    name: "lifecycle.unsupported_named",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      const refused: UnsupportedError[] = [];
      const attempts: Array<[string, () => Promise<unknown>]> = [
        ["pause", () => sandbox.pause().then(() => sandbox.resume())],
        ["fork", () => sandbox.fork({ name: ctx.name("fork") }).then((fork) => fork.remove({ force: true }))],
      ];
      for (const [verb, attempt] of attempts) {
        try {
          await attempt();
        } catch (err) {
          if (!(err instanceof UnsupportedError)) {
            throw err;
          }
          assert.match(err.message, new RegExp(verb), `the refusal names ${verb}`);
          refused.push(err);
        }
      }
      if (refused.length === 0) {
        skip("the provider runs every optional verb");
      }
      for (const err of refused) {
        assert.equal(err.code, "unsupported");
      }
    },
  },
  {
    name: "lifecycle.capabilities",
    run: async (ctx) => {
      const capabilities = await ctx.shard.capabilities();
      assert.deepEqual(Object.keys(capabilities).sort(), ["create", "fork", "pause", "port", "remove", "resume", "snapshot", "start", "stop", "swap"]);
      for (const verb of ["create", "start", "stop", "remove", "port"] as const) {
        assert.equal(capabilities[verb], true, `every provider can ${verb}`);
      }
      assert.equal(capabilities.resume, capabilities.pause, "pause and resume come as a pair");
      const sandbox = await ctx.create();
      await agrees("pause", capabilities.pause, () => sandbox.pause().then(() => sandbox.resume()));
      await agrees("fork", capabilities.fork, () => sandbox.fork({ name: ctx.name("fork") }).then((fork) => fork.remove({ force: true })));
      await sandbox.stop();
      await agrees("snapshot", capabilities.snapshot, () => ctx.snapshot(sandbox));
    },
  },
];
