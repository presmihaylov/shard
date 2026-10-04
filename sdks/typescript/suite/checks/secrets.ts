import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { ConflictError, NotFoundError } from "useshards";
import { rejects, type Check } from "../harness.js";

const destinations = ["api.example.com"];

export const checks: Check[] = [
  {
    name: "secrets.set_list_remove",
    run: async (ctx) => {
      const name = ctx.variable("secret");
      const value = `suite-value-${randomBytes(8).toString("hex")}`;
      await ctx.secret(name, { value, destinations });
      const listed = await ctx.shard.secrets.list();
      const entry = listed.find((each) => each.name === name);
      assert.ok(entry, "list holds the secret");
      assert.deepEqual(entry.destinations, destinations);
      assert.equal(entry.placeholder, `mock-${name}`);
      assert.ok(entry.updatedAt instanceof Date);
      assert.ok(!JSON.stringify(listed).includes(value), "a list never carries a value");
      await ctx.shard.secrets.set(name, { value: `${value}-rotated`, destinations });
      const rotated = (await ctx.shard.secrets.list()).find((each) => each.name === name);
      assert.equal(rotated?.placeholder, `mock-${name}`, "a rotation keeps the placeholder");
      await ctx.shard.secrets.remove(name);
      assert.ok(!(await ctx.shard.secrets.list()).some((each) => each.name === name));
      await rejects(NotFoundError, () => ctx.shard.secrets.remove(name));
    },
  },
  {
    name: "secrets.grant_ungrant",
    run: async (ctx) => {
      const name = ctx.variable("grant");
      const value = `suite-value-${randomBytes(8).toString("hex")}`;
      await ctx.secret(name, { value, destinations });
      const sandbox = await ctx.create();
      await rejects(ConflictError, () => ctx.shard.secrets.grant(sandbox, name));
      await sandbox.stop();
      await ctx.shard.secrets.grant(sandbox, name);
      assert.deepEqual((await sandbox.inspect()).secrets, [name]);
      await sandbox.start();
      const env = await sandbox.exec(`printenv ${name}`);
      assert.equal(env.stdout, `mock-${name}\n`, "the guest holds the placeholder");
      assert.ok(!env.stdout.includes(value), "the guest never holds the value");
      await rejects(ConflictError, () => ctx.shard.secrets.remove(name));
      await sandbox.stop();
      await ctx.shard.secrets.ungrant(sandbox, name);
      assert.deepEqual((await sandbox.inspect()).secrets, []);
      await sandbox.start();
      assert.equal((await sandbox.exec(`printenv ${name}`)).exitCode, 1, "an ungrant takes the placeholder back");
      const holder = await ctx.create({ secrets: [name] });
      assert.equal((await holder.exec(`printenv ${name}`)).stdout, `mock-${name}\n`, "a create grants it too");
    },
  },
];
