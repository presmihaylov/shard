// One step of a cross-SDK check, in a process of its own, so the next step shares no client state with this one.
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { Shard } from "useshards";

const [step, ...args] = process.argv.slice(2);
const shard = new Shard();

/** printed resolves once the command's stdout has carried the text, so a step acts only on a command that runs. */
function printed(text) {
  let seen = "";
  let resolve;
  const once = new Promise((r) => (resolve = r));
  const onStdout = (chunk) => {
    seen += Buffer.from(chunk).toString("utf8");
    if (seen.includes(text)) {
      resolve();
    }
  };

  return { once, onStdout };
}

const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");

const steps = {
  async create(image, name) {
    const sandbox = await shard.create({ image, name });

    return { id: sandbox.id };
  },

  async remove(id) {
    await (await shard.get(id)).remove({ force: true });

    return {};
  },

  async exec(id, script) {
    const result = await (await shard.get(id)).exec(script);

    return { exit_code: result.exitCode, stdout: result.stdout, stderr: result.stderr };
  },

  // A foreground exec this client aborts once it runs; the script writes the marker file only if it outlives the abort.
  async cancel(id, marker) {
    const sandbox = await shard.get(id);
    const started = printed("started");
    const controller = new AbortController();
    const running = sandbox.exec(`echo started; sleep 8; echo ${marker} > /tmp/${marker}`, {
      signal: controller.signal,
      onStdout: started.onStdout,
    });
    await Promise.race([started.once, running]);
    controller.abort();
    const err = await running.then(
      () => new Error("the exec ended before the abort"),
      (caught) => caught,
    );

    return { cancelled: err.name };
  },

  // A second client finds the command another process started by its marker, and reads it to its end.
  async find(id, marker) {
    const sandbox = await shard.get(id);
    const entry = (await sandbox.commands.list()).find((each) => each.command.join(" ").includes(marker));
    if (!entry) {
      return { state_at_list: null };
    }
    const result = await (await sandbox.commands.get(entry.id)).wait();
    const file = await sandbox.files.read(`/tmp/${marker}`, { encoding: "utf8" });

    return { state_at_list: entry.state, exit_code: result.exitCode, stdout: result.stdout, file };
  },

  // A background command this client lets go of once it printed, so another client can reconnect to it.
  async start(id, marker) {
    const sandbox = await shard.get(id);
    const one = printed("one");
    const command = await sandbox.exec(`echo one; sleep 4; echo ${marker}`, { background: true, onStdout: one.onStdout });
    await one.once;
    command.disconnect();

    return { command: command.id };
  },

  async attach(id, command) {
    const result = await (await (await shard.get(id)).commands.get(command)).wait();

    return { exit_code: result.exitCode, stdout: result.stdout };
  },

  async capture(id, script, limit) {
    let seen = 0;
    const result = await (await shard.get(id)).exec(script, {
      outputLimitBytes: Number(limit),
      onStdout: (chunk) => (seen += chunk.length),
    });
    const stdout = Buffer.from(result.stdout, "utf8");

    return {
      exit_code: result.exitCode,
      seen,
      lost_bytes: result.lostBytes,
      stdout_len: stdout.length,
      stdout_sha256: sha256(stdout),
      stderr_len: Buffer.byteLength(result.stderr),
    };
  },

  async put(id, local, remote) {
    await (await shard.get(id)).files.write(remote, await readFile(local));

    return {};
  },

  async get(id, remote) {
    const bytes = await (await shard.get(id)).files.read(remote);

    return { size: bytes.length, sha256: sha256(bytes) };
  },
};

async function main() {
  const run = steps[step];
  if (!run) {
    throw new Error(`no step ${step}; want one of ${Object.keys(steps).join(", ")}`);
  }
  console.log(JSON.stringify(await run(...args)));
}

// The exit waits for stdout to drain, which a pipe does not do on its own, then drops any socket the client holds.
main()
  .then(
    () => 0,
    (err) => {
      console.error(`actor ${step}: ${err?.stack ?? err}`);

      return 1;
    },
  )
  .then((code) => {
    shard.close();
    process.stdout.write("", () => process.exit(code));
  });
