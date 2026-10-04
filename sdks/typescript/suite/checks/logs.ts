import assert from "node:assert/strict";
import type { NetworkLogRecord, Sandbox } from "useshards";
import { consume, text, waitFor, type Check, type Context } from "../harness.js";

/** denied is a running sandbox under a policy that denies everything, so any request it makes leaves a deny record. */
async function denied(ctx: Context): Promise<Sandbox> {
  const policy = ctx.name("deny");
  await ctx.policy(policy, [{ action: "deny", rule: "any" }]);

  return ctx.create({ policy });
}

async function request(sandbox: Sandbox, host: string): Promise<void> {
  const result = await sandbox.exec(`wget -q -T 5 -O /dev/null http://${host}/`);
  assert.notEqual(result.exitCode, 0, `the policy let ${host} through`);
}

function deniedHost(records: NetworkLogRecord[], host: string): NetworkLogRecord | undefined {
  return records.find((record) => record.verdict === "deny" && record.host === host);
}

export const checks: Check[] = [
  {
    name: "logs.read",
    run: async (ctx) => {
      const app = await ctx.run("echo line-one; echo line-two >&2; sleep 300");
      let logs = "";
      await waitFor("both lines in the logs", 15_000, async () => {
        logs = await app.sandbox.logs();

        return logs.includes("line-two\n");
      });
      assert.ok(logs.includes("line-one\n"), `the logs hold stdout: ${JSON.stringify(logs)}`);
    },
  },
  {
    name: "logs.follow",
    run: async (ctx) => {
      const app = await ctx.run("for i in 1 2 3; do echo tick $i; sleep 1; done; sleep 300");
      let out = "";
      for await (const chunk of app.sandbox.followLogs()) {
        out += text(chunk);
        if (out.includes("tick 3\n")) {
          break;
        }
      }
      assert.match(out, /tick 1\n[\s\S]*tick 2\n[\s\S]*tick 3\n/, "the follow yields the lines as they come");

      let rest = "";
      const ended = consume(app.sandbox.followLogs(), (chunk) => {
        rest += text(chunk);
      });
      await waitFor("the second follow to replay the log", 10_000, async () => rest.includes("tick 3\n"));
      assert.ok(rest.startsWith("tick 1\n"), "a follow starts at the beginning of the log");
      await app.sandbox.stop();
      assert.equal(await ended, undefined, "a stop ends the follow without an error");
    },
  },
  {
    name: "logs.network_read",
    run: async (ctx) => {
      const sandbox = await denied(ctx);
      const host = `${ctx.name("host")}.example`;
      await request(sandbox, host);
      let records: NetworkLogRecord[] = [];
      await waitFor(`a deny record for ${host}`, 10_000, async () => {
        records = await sandbox.networkLogs();

        return deniedHost(records, host) !== undefined;
      });
      const record = deniedHost(records, host);
      assert.ok(record?.time instanceof Date && !Number.isNaN(record.time.getTime()));
      assert.ok(record.rule.length > 0, "a record names the rule that decided it");
    },
  },
  {
    name: "logs.network_follow",
    run: async (ctx) => {
      const sandbox = await denied(ctx);
      const host = `${ctx.name("host")}.example`;
      const controller = new AbortController();
      const seen: NetworkLogRecord[] = [];
      const ended = consume(sandbox.followNetworkLogs({ signal: controller.signal }), (record) => seen.push(record));
      try {
        await request(sandbox, host);
        await waitFor(`the follow to yield a deny record for ${host}`, 10_000, async () => deniedHost(seen, host) !== undefined);
      } finally {
        controller.abort();
      }
      const err = await ended;
      assert.ok(err instanceof Error && err.name === "AbortError", "an abort ends the follow with the signal's reason");
    },
  },
];
