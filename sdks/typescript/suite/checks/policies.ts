import assert from "node:assert/strict";
import { ConflictError, InvalidRequestError, NotFoundError, type PolicyRule } from "useshards";
import { rejects, type Check } from "../harness.js";

// Each rule is spelled as the daemon spells it back, so a round trip compares equal.
const rules: PolicyRule[] = [
  { action: "allow", rule: "suffix:example.com tcp:443" },
  { action: "allow", rule: "10.0.0.0/8 tcp:22" },
  { action: "deny", rule: "any" },
];

export const checks: Check[] = [
  {
    name: "policies.set_get_list_remove",
    run: async (ctx) => {
      const name = ctx.name("policy");
      const set = await ctx.policy(name, rules);
      assert.equal(set.name, name);
      assert.deepEqual(set.rules, rules);
      assert.deepEqual((await ctx.shard.policies.get(name)).rules, rules, "the rules come back in order");
      assert.ok((await ctx.shard.policies.list()).some((each) => each.name === name), "list holds the policy");
      const replaced: PolicyRule[] = [{ action: "deny", rule: "any" }];
      await ctx.shard.policies.set(name, replaced);
      assert.deepEqual((await ctx.shard.policies.get(name)).rules, replaced, "a set replaces every rule");
      await rejects(InvalidRequestError, () => ctx.shard.policies.set(ctx.name("bad"), [{ action: "allow", rule: "not a rule at all" }]));
      await ctx.shard.policies.remove(name);
      await rejects(NotFoundError, () => ctx.shard.policies.get(name));
    },
  },
  {
    name: "policies.attach_detach",
    run: async (ctx) => {
      const name = ctx.name("policy");
      await ctx.policy(name, rules);
      const sandbox = await ctx.create();
      assert.equal(sandbox.info.policy, null);
      await rejects(ConflictError, () => ctx.shard.policies.attach(sandbox, name));
      await sandbox.stop();
      await ctx.shard.policies.attach(sandbox, name);
      assert.equal((await sandbox.inspect()).policy, name);
      await rejects(ConflictError, () => ctx.shard.policies.remove(name));
      await ctx.shard.policies.detach(sandbox);
      assert.equal((await sandbox.inspect()).policy, null);
    },
  },
];
