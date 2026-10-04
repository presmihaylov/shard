import assert from "node:assert/strict";
import { setTimeout as sleep } from "node:timers/promises";
import { type Check } from "../harness.js";

export const checks: Check[] = [
  {
    name: "apps.run_handle",
    run: async (ctx) => {
      const command = ["sh", "-c", "echo up; sleep 300"];
      const app = await ctx.run(command);
      assert.ok(app.sandbox.id.length > 0, "the handle holds its sandbox");
      assert.equal(app.sandbox.info.state, "running");
      const info = await app.inspect();
      assert.deepEqual(info.command, command);
      assert.equal(info.exitStatus, null, "a running app has no exit yet");
      assert.equal(info.restart, null, "run with no restart option sets no policy");
      assert.deepEqual((await app.sandbox.inspect()).app?.command, command, "the sandbox record names its app");
    },
  },
  {
    name: "apps.sandbox_exec",
    run: async (ctx) => {
      const app = await ctx.run(["sleep", "300"]);
      const result = await app.sandbox.exec(["ps", "-o", "args"]);
      assert.equal(result.exitCode, 0, result.stderr);
      assert.ok(result.stdout.split("\n").includes("sleep 300"), "a command in the app's sandbox sees the app");
    },
  },
  {
    name: "apps.inspect_logs_wait",
    run: async (ctx) => {
      const app = await ctx.run("echo out; echo err >&2; exit 4");
      const exit = await app.wait();
      assert.equal(exit.exitCode, 4);
      assert.equal(exit.signal, null);
      assert.equal(exit.restarts, 0);
      assert.deepEqual((await app.inspect()).exitStatus, { code: 4, signal: null });
      const logs = await app.logs();
      assert.ok(logs.includes("out\n") && logs.includes("err\n"), `the logs hold both streams: ${JSON.stringify(logs)}`);
      assert.equal((await app.sandbox.inspect()).state, "running", "the sandbox outlives its app");
      assert.equal((await app.sandbox.exec("echo after")).stdout, "after\n");
    },
  },
  {
    name: "apps.stop_keeps_sandbox",
    run: async (ctx) => {
      // always would start a TERMed app again, so the stop must cancel the policy, not only signal the app.
      const app = await ctx.run(["sleep", "300"], { restart: { policy: "always", backoff: 1 } });
      await app.stop();
      const exit = await app.wait();
      assert.equal(exit.signal, 15, "stop ends the app with TERM");
      assert.equal(exit.restarts, 0);
      await sleep(3000);
      const { restart } = await app.inspect();
      assert.ok(restart, "the app reports its policy");
      assert.equal(restart.count, 0, "no start again follows the stop, past the backoff");
      assert.equal(restart.ended, true);
      assert.equal((await app.sandbox.inspect()).state, "running", "stop ends the app and keeps the sandbox");
      const ps = await app.sandbox.exec(["ps", "-o", "args"]);
      assert.ok(!ps.stdout.split("\n").includes("sleep 300"), `the app is gone: ${JSON.stringify(ps.stdout)}`);
    },
  },
  {
    name: "apps.restart_policy",
    run: async (ctx) => {
      const app = await ctx.run("echo start; exit 1", { restart: { policy: "on-failure", retries: 2, backoff: 1 } });
      const exit = await app.wait();
      assert.equal(exit.exitCode, 1);
      assert.equal(exit.restarts, 2, "wait answers once the retries are spent");
      const { restart } = await app.inspect();
      assert.ok(restart, "the app reports its policy");
      assert.equal(restart.policy, "on-failure");
      assert.equal(restart.retries, 2);
      assert.equal(restart.count, 2);
      assert.equal(restart.gaveUp, true);
      assert.equal(restart.ended, true);
      assert.equal((await app.logs()).match(/^start$/gm)?.length, 3, "the first start and two more");
    },
  },
];
