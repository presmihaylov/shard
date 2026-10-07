import assert from "node:assert/strict";
import { setTimeout as sleep } from "node:timers/promises";
import { ConflictError, NotFoundError } from "useshards";
import { rejects, type Check } from "../harness.js";

export const checks: Check[] = [
  {
    name: "processes.run_handle",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      const command = ["sh", "-c", "echo up; sleep 300"];
      const web = await sandbox.run(command, { name: "web" });
      assert.equal(web.name, "web");
      assert.deepEqual(web.info.command, command);
      assert.equal(web.info.status.state, "running");
      assert.equal(web.info.status.exit, null, "a running process has no exit yet");
      assert.equal(web.info.restart.policy, "unless-stopped", "a run with no restart option is unless-stopped");
      assert.deepEqual(
        (await sandbox.inspect()).processes.map((each) => each.name),
        ["web"],
        "the sandbox record lists its process",
      );
      assert.equal((await sandbox.run(["sleep", "300"])).name, "sleep", "a run with no name takes its program's base name");
    },
  },
  {
    name: "processes.list_get",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      await sandbox.run(["sleep", "300"], { name: "first" });
      await sandbox.run(["sleep", "301"], { name: "second" });
      assert.deepEqual(
        (await sandbox.processes.list()).map((each) => each.name),
        ["first", "second"],
        "list answers in run order",
      );
      assert.equal((await sandbox.processes.get("second")).info.status.state, "running");
      const ps = await sandbox.exec(["ps", "-o", "args"]);
      assert.ok(ps.stdout.split("\n").includes("sleep 301"), "a command in the sandbox sees its processes");
      assert.equal((await rejects(NotFoundError, () => sandbox.processes.get("none"))).code, "no_process");
      assert.equal((await rejects(ConflictError, () => sandbox.run(["sleep", "300"], { name: "first" }))).code, "name_taken");
    },
  },
  {
    name: "processes.wait_logs",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      const job = await sandbox.run("echo out; echo err >&2; exit 4", { restart: { policy: "no" } });
      const ended = await job.wait();
      assert.equal(ended.status.state, "exited");
      assert.deepEqual(ended.status.exit, { exitCode: 4, signal: null });
      const logs = await job.logs();
      assert.ok(logs.includes("out\n") && logs.includes("err\n"), `the logs hold both streams: ${JSON.stringify(logs)}`);
      assert.equal((await sandbox.inspect()).state, "running", "the sandbox outlives its process");
      assert.equal((await sandbox.exec("echo after")).stdout, "after\n");
    },
  },
  {
    name: "processes.kill_keeps_sandbox",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      // always would start a TERMed process again, so the kill must cancel the policy, not only signal the process.
      const web = await sandbox.run(["sleep", "300"], { restart: { policy: "always", backoff: 1 } });
      const killed = await web.kill();
      assert.equal(killed.status.state, "killed");
      assert.equal(killed.status.exit?.signal, 15, "kill ends the process with TERM");
      assert.equal(killed.killed, true);
      await sleep(3000);
      const after = await web.inspect();
      assert.equal(after.status.state, "killed", "no start again follows the kill, past the backoff");
      assert.equal(after.status.restarts, 0);
      assert.equal((await sandbox.inspect()).state, "running", "kill ends the process and keeps the sandbox");
      const ps = await sandbox.exec(["ps", "-o", "args"]);
      assert.ok(!ps.stdout.split("\n").includes("sleep 300"), `the process is gone: ${JSON.stringify(ps.stdout)}`);
    },
  },
  {
    name: "processes.restart_policy",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      const job = await sandbox.run("echo start; exit 1", { restart: { policy: "on-failure", retries: 2, backoff: 1 } });
      const ended = await job.wait();
      assert.equal(ended.status.state, "gave-up", "wait answers once the retries are spent");
      assert.equal(ended.status.exit?.exitCode, 1);
      assert.equal(ended.status.restarts, 2);
      assert.deepEqual(ended.restart, { policy: "on-failure", retries: 2, backoff: 1 });
      assert.equal((await job.logs()).match(/^start$/gm)?.length, 3, "the first start and two more");
    },
  },
  {
    name: "processes.run_again",
    run: async (ctx) => {
      const sandbox = await ctx.create();
      await (await sandbox.run("echo ran", { name: "job", restart: { policy: "no" } })).wait();
      const again = await sandbox.run("echo ran", { name: "job", restart: { policy: "no" } });
      await again.wait();
      assert.equal((await again.logs()).match(/^ran$/gm)?.length, 2, "a run of an ended name appends to its log");
      assert.equal((await sandbox.processes.list()).length, 1, "the run takes the ended entry's place");
    },
  },
];
