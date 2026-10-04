import assert from "node:assert/strict";
import { CommandNotStartedError, type Command } from "useshards";
import { rejects, text, waitFor, type Check } from "../harness.js";

/** printed resolves once a command's stdout holds the given text, so a check acts at a known point of the run. */
function printed(want: string): { onStdout: (chunk: Uint8Array) => void; seen: Promise<void> } {
  let out = "";
  let resolve: () => void = () => {};
  const seen = new Promise<void>((done) => {
    resolve = done;
  });

  return {
    onStdout: (chunk) => {
      out += text(chunk);
      if (out.includes(want)) {
        resolve();
      }
    },
    seen,
  };
}

async function state(command: Command): Promise<string> {
  return (await command.inspect()).state;
}

export const checks: Check[] = [
  {
    name: "commands.argv",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const result = await sandbox.exec(["printf", "%s", "hello world"]);
      assert.equal(result.exitCode, 0);
      assert.equal(result.stdout, "hello world", "an array runs as argv, with no shell to split it");
      assert.equal(result.stderr, "");
    },
  },
  {
    name: "commands.shell",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const result = await sandbox.exec("printf '%s' hello; echo oops >&2; echo $((1 + 2))");
      assert.equal(result.exitCode, 0);
      assert.equal(result.stdout, "hello3\n", "a string runs under /bin/sh -c");
      assert.equal(result.stderr, "oops\n");
    },
  },
  {
    name: "commands.env_workdir_user",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const result = await sandbox.exec('echo "$GREETING"; pwd; id -u', {
        env: { GREETING: "hi there" },
        workdir: "/tmp",
        user: "nobody",
      });
      assert.equal(result.stdout, "hi there\n/tmp\n65534\n");
    },
  },
  {
    name: "commands.stdin",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      assert.equal((await sandbox.exec(["cat"], { stdin: "from a string\n" })).stdout, "from a string\n");
      const bytes = new Uint8Array([0, 1, 2, 250, 255]);
      const result = await sandbox.exec("od -An -tu1", { stdin: bytes });
      assert.equal(result.stdout.trim().split(/\s+/).join(","), "0,1,2,250,255", "stdin carries bytes as they are");
      const open = await sandbox.exec(["cat"], { background: true, stdin: true });
      await open.writeStdin("first ");
      await open.writeStdin(new TextEncoder().encode("second"));
      await open.closeStdin();
      assert.equal((await open.wait()).stdout, "first second", "close ends the input, so cat ends");
    },
  },
  {
    name: "commands.tty_resize",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const first = printed("24 80");
      const command = await sandbox.exec("stty size; read line; stty size", {
        background: true,
        stdin: true,
        tty: { rows: 24, cols: 80 },
        onStdout: first.onStdout,
      });
      await first.seen;
      await command.resize(40, 120);
      await command.writeStdin("\n");
      const result = await command.wait();
      assert.equal(result.exitCode, 0);
      assert.match(result.stdout, /24 80\r?\n[\s\S]*40 120/, "the second stty sees the new size");
      assert.equal(result.stderr, "", "a terminal sends everything as stdout");
    },
  },
  {
    name: "commands.nonzero_result",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const result = await sandbox.exec("echo partial; exit 3");
      assert.equal(result.exitCode, 3, "a nonzero exit is a result, not an error");
      assert.equal(result.stdout, "partial\n");
      assert.equal((await sandbox.exec(["false"])).exitCode, 1);
    },
  },
  {
    name: "commands.not_started_named",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const err = await rejects(CommandNotStartedError, () => sandbox.exec(["/no/such/binary"]));
      assert.equal(err.exitCode, 127);
      assert.match(err.reason, /no\/such\/binary/);
      const result = await sandbox.exec("/no/such/binary");
      assert.equal(result.exitCode, 127, "the shell started, so a missing binary under it is a result");
    },
  },
  {
    name: "commands.background_wait_kill",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const quick = await sandbox.exec("sleep 1; echo done", { background: true });
      assert.ok(quick.id.length > 0, "a background command has an id once it started");
      const result = await quick.wait();
      assert.equal(result.exitCode, 0);
      assert.equal(result.stdout, "done\n");
      const long = await sandbox.exec(["sleep", "300"], { background: true });
      assert.equal(await state(long), "running");
      await long.kill();
      // Every provider reports a signal as the exit code 128 plus its number, and no signal (SHARD-432).
      const killed = await long.wait();
      assert.equal(killed.exitCode, 143, "kill sends TERM by default");
      assert.equal(killed.signal, null);
      const hard = await sandbox.exec(["sleep", "300"], { background: true });
      await hard.kill("KILL");
      const hardKilled = await hard.wait();
      assert.equal(hardKilled.exitCode, 137);
      assert.equal(hardKilled.signal, null);
    },
  },
  {
    name: "commands.list_get",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const command = await sandbox.exec(["sleep", "300"], { background: true });
      const listed = await sandbox.commands.list();
      const entry = listed.find((each) => each.id === command.id);
      assert.ok(entry, "list holds the running command");
      assert.deepEqual(entry.command, ["sleep", "300"]);
      assert.equal(entry.state, "running");
      const got = await sandbox.commands.get(command.id);
      assert.equal(got.id, command.id);
      assert.equal(await state(got), "running");
      await got.kill();
      await waitFor("the killed command to end", 10_000, async () => (await state(command)) === "exited");
      const ended = (await sandbox.commands.list()).find((each) => each.id === command.id);
      assert.equal(ended?.exitCode, 143, "kill sends TERM by default");
      assert.equal(ended?.signal, null);
    },
  },
  {
    name: "commands.reconnect",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const one = printed("one");
      const command = await sandbox.exec("echo one; sleep 2; echo two", { background: true, onStdout: one.onStdout });
      await one.seen;
      command.disconnect();
      await command.reconnect();
      const result = await command.wait();
      assert.equal(result.exitCode, 0);
      assert.equal(result.stdout, "one\ntwo\n", "a reconnect replays what the daemon holds, then streams the rest");
    },
  },
  {
    name: "commands.disconnect",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const marker = ctx.name("disconnect");
      const command = await sandbox.exec(`sleep 2; echo ${marker} > /tmp/${marker}`, { background: true });
      command.disconnect();
      assert.equal(await state(command), "running", "a disconnect leaves the command running");
      await waitFor("the disconnected command to end", 15_000, async () => (await state(command)) === "exited");
      assert.equal(await sandbox.files.read(`/tmp/${marker}`, { encoding: "utf8" }), `${marker}\n`);
    },
  },
  {
    name: "commands.cancel_keeps_remote",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const marker = ctx.name("cancel");
      const started = printed("started");
      const controller = new AbortController();
      const running = sandbox.exec(`echo started; sleep 3; echo ${marker} > /tmp/${marker}`, {
        signal: controller.signal,
        onStdout: started.onStdout,
      });
      // The race surfaces a command that failed before it printed, instead of waiting on it forever.
      await Promise.race([started.seen, running]);
      controller.abort();
      const err = await rejects(Error, () => running);
      assert.equal(err.name, "AbortError", "an abort rejects with the signal's reason");

      // A second client, as another process would make one, finds the command still running.
      const other = await ctx.client().get(sandbox.id);
      const entry = (await other.commands.list()).find((each) => each.command.join(" ").includes(marker));
      assert.ok(entry, "the second client lists the cancelled command");
      assert.equal(entry.state, "running", "a client cancel leaves the remote command running");
      const result = await (await other.commands.get(entry.id)).wait();
      assert.equal(result.exitCode, 0);
      assert.equal(result.stdout, "started\n");
      assert.equal(await other.files.read(`/tmp/${marker}`, { encoding: "utf8" }), `${marker}\n`);
    },
  },
];
