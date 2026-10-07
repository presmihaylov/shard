import assert from "node:assert/strict";
import { connect, createServer } from "node:net";
import { ConflictError, NotFoundError } from "useshards";
import { rejects, skip, waitFor, type Check } from "../harness.js";

const guestPort = 8000;

/** onThisHost is whether SHARD_REMOTE names this host, where a forward's 127.0.0.1 is the suite's own. */
function onThisHost(): boolean {
  return ["127.0.0.1", "localhost", "[::1]"].includes(new URL(process.env.SHARD_REMOTE ?? "").hostname);
}

/** freePort is a port nothing on this host listens on now, so the daemon may bind it next. */
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = createServer();
    probe.on("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const address = probe.address();
      if (address === null || typeof address === "string") {
        reject(new Error(`a TCP listener has no port: ${String(address)}`));
        return;
      }
      probe.close(() => resolve(address.port));
    });
  });
}

/** greeting is the first line the guest sends through a host port of this host, or what it sent before it closed. */
function greeting(hostPort: number): Promise<string> {
  return new Promise((resolve, reject) => {
    let got = "";
    const socket = connect({ host: "127.0.0.1", port: hostPort });
    socket.setEncoding("utf8");
    socket.setTimeout(5_000, () => socket.destroy(new Error(`host port ${hostPort} sent no line within 5 s`)));
    socket.on("data", (chunk: string) => {
      got += chunk;
      if (got.includes("\n")) {
        socket.destroy();
        resolve(got);
      }
    });
    socket.on("end", () => resolve(got));
    socket.on("error", reject);
  });
}

export const checks: Check[] = [
  {
    name: "ports.add_connect_remove",
    run: async (ctx) => {
      if (!onThisHost()) {
        skip(`SHARD_REMOTE names another host, and a private forward listens on that host's 127.0.0.1 alone`);
      }
      const sandbox = await ctx.create();
      const marker = ctx.name("port");
      // A loop, since busybox nc answers one connection and a probe of the guest port may take it.
      const listener = await sandbox.exec(`while true; do echo ${marker} | nc -l -p ${guestPort}; done`, { background: true });
      listener.disconnect();
      const hostPort = await freePort();
      const added = await sandbox.ports.add(hostPort, guestPort);
      assert.equal(added.sandbox, sandbox.id);
      assert.deepEqual([added.hostPort, added.guestPort, added.public, added.address], [hostPort, guestPort, false, "127.0.0.1"]);
      assert.equal(added.listening, true, `a running sandbox listens at once: ${added.error}`);
      assert.ok(added.reachableOn.length > 0 && added.reachableOn.every((on) => on.address === "127.0.0.1"), "a private forward is reachable on loopback alone");
      await waitFor("the guest line through the host port", 30_000, async () => (await greeting(hostPort).catch(() => "")) === `${marker}\n`);
      assert.deepEqual(
        (await sandbox.ports.list()).map((each) => [each.hostPort, each.guestPort, each.listening]),
        [[hostPort, guestPort, true]],
      );
      assert.ok((await ctx.shard.ports()).some((each) => each.hostPort === hostPort && each.sandbox === sandbox.id), "the list of every forward holds it");
      assert.deepEqual((await sandbox.inspect()).ports, [{ hostPort, guestPort, public: false }]);
      await sandbox.ports.remove(hostPort);
      assert.deepEqual(await sandbox.ports.list(), []);
      const refused = await rejects(Error, () => greeting(hostPort));
      assert.match(refused.message, /ECONNREFUSED/, "a removed forward listens on nothing");
    },
  },
  {
    name: "ports.held_by_one_sandbox",
    run: async (ctx) => {
      const owner = await ctx.create();
      await owner.stop();
      // A stopped sandbox binds nothing, so any host port works, on this host or another.
      const hostPort = await freePort();
      const added = await owner.ports.add(hostPort, guestPort, { public: true });
      assert.deepEqual([added.public, added.address, added.listening, added.reachableOn], [true, "0.0.0.0", false, []]);
      assert.deepEqual((await owner.inspect()).ports, [{ hostPort, guestPort, public: true }]);
      const other = await ctx.sandbox();
      await rejects(ConflictError, () => other.ports.add(hostPort, guestPort));
      await rejects(NotFoundError, () => other.ports.remove(hostPort));
      await owner.ports.remove(hostPort);
      assert.deepEqual((await owner.inspect()).ports, []);
      await rejects(NotFoundError, () => owner.ports.remove(hostPort));
    },
  },
];
