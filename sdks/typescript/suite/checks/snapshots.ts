import assert from "node:assert/strict";
import { ConflictError, NotFoundError, type Sandbox } from "useshards";
import { rejects, type Check, type Context } from "../harness.js";

/** stoppedSource is a stopped sandbox with a marker file, the one state a snapshot takes. */
async function stoppedSource(ctx: Context): Promise<Sandbox> {
  const source = await ctx.create();
  await source.exec("echo snapshot > /root/marker");
  await source.stop();

  return source;
}

export const checks: Check[] = [
  {
    name: "snapshots.needs_stopped",
    run: async (ctx) => {
      const running = await ctx.sandbox();
      await rejects(ConflictError, () => ctx.shard.snapshots.create(running, { name: ctx.name("snapshot") }));
    },
  },
  {
    name: "snapshots.create_list_inspect",
    run: async (ctx) => {
      const source = await ctx.fixture("snapshot-source", stoppedSource);
      const snapshot = await ctx.snapshot(source);
      assert.equal(snapshot.source, source.id);
      assert.ok(snapshot.name?.startsWith(ctx.prefix), `name ${snapshot.name}`);
      const listed = await ctx.shard.snapshots.list();
      assert.ok(listed.some((each) => each.id === snapshot.id), "list holds the snapshot");
      assert.equal((await ctx.shard.snapshots.inspect(snapshot.id)).name, snapshot.name);
      assert.ok(snapshot.name, "the harness names every snapshot");
      assert.equal((await ctx.shard.snapshots.inspect(snapshot.name)).id, snapshot.id);
    },
  },
  {
    name: "snapshots.create_from",
    run: async (ctx) => {
      const source = await ctx.fixture("snapshot-source", stoppedSource);
      const snapshot = await ctx.snapshot(source);
      const sandbox = await ctx.create({ image: undefined, snapshot: snapshot.id });
      assert.equal(sandbox.info.state, "running");
      assert.equal((await sandbox.exec("cat /root/marker")).stdout, "snapshot\n", "the new sandbox holds the snapshot's files");
    },
  },
  {
    name: "snapshots.outlive_source",
    run: async (ctx) => {
      const source = await stoppedSource(ctx);
      const snapshot = await ctx.snapshot(source);
      await source.remove();
      await rejects(NotFoundError, () => source.inspect());
      assert.equal((await ctx.shard.snapshots.inspect(snapshot.id)).id, snapshot.id, "the snapshot outlives its source");
      const sandbox = await ctx.create({ image: undefined, snapshot: snapshot.id });
      assert.equal((await sandbox.exec("cat /root/marker")).stdout, "snapshot\n");
    },
  },
  {
    name: "snapshots.remove",
    run: async (ctx) => {
      const source = await ctx.fixture("snapshot-source", stoppedSource);
      const snapshot = await ctx.snapshot(source);
      await ctx.shard.snapshots.remove(snapshot.id);
      await rejects(NotFoundError, () => ctx.shard.snapshots.inspect(snapshot.id));
      assert.ok(!(await ctx.shard.snapshots.list()).some((each) => each.id === snapshot.id));
    },
  },
];
