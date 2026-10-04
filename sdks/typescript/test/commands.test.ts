import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { Commands } from "../src/commands.js";
import { ConnectionError, NotFoundError, ProtocolError } from "../src/errors.js";
import { opBinary, opClose } from "../src/frames.js";
import { Transport } from "../src/transport.js";
import { FakeDaemon, type Answer, type Peer, type Request } from "./helpers/daemon.js";

const execs = "/v0/sandboxes/sb_1/exec";

function record(fields: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    exec: "ex_1",
    sandbox: "sb_1",
    command: ["/bin/sh", "-c", "echo hi"],
    state: "running",
    exit_status: null,
    started_at: "2026-10-04T10:00:00Z",
    exited_at: null,
    truncated: false,
    lost_bytes: 0,
    ...fields,
  };
}

let daemon: FakeDaemon;
let transport: Transport;
let commands: Commands;
let routes: Map<string, (request: Request) => Answer>;

beforeEach(async () => {
  daemon = await FakeDaemon.start();
  transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined });
  commands = new Commands(transport, "sb_1");
  routes = new Map([[`POST ${execs}`, () => ({ status: 201, json: record() })]]);
  daemon.route = (request) => routes.get(`${request.method} ${request.url.pathname}`)?.(request);
  daemon.upgrade = () => undefined;
});

afterEach(async () => {
  transport.close();
  await daemon.close();
});

/** started answers the body of the n-th command start the daemon got. */
function started(index = 0): Record<string, unknown> {
  const create = daemon.requests.filter((r) => r.method === "POST" && r.url.pathname === execs)[index];
  assert.ok(create, "the command was started");

  return JSON.parse(create.body);
}

/** Broken is input this client fails to send, for a reason of its own rather than the stream's. */
class Broken extends Uint8Array {
  override subarray(): never {
    throw new Error("local failure");
  }
}

/** stdinOf reads the client's frames until stdin closes, and answers what it sent. */
async function stdinOf(peer: Peer): Promise<string> {
  let sent = "";
  for (;;) {
    const frame = await peer.next();
    assert.equal(frame?.opcode, opBinary);
    if (frame?.payload[0] === 4) {
      return sent;
    }
    sent += frame?.payload.subarray(1).toString();
  }
}

test("run sends the settings and answers the exit with the output as text", async () => {
  const running = commands.run(["printf", "%s", "hi"], { env: { A: "1" }, workdir: "/tmp", user: "nobody", tty: { rows: 24, cols: 80 } });
  const peer = await daemon.peer(0);
  peer.send(1, "héllo");
  peer.send(2, "oops");
  peer.exit({ code: 3, lost_bytes: 2 });
  assert.deepEqual(await running, { exitCode: 3, signal: null, lostBytes: 2, stdout: "héllo", stderr: "oops" });
  assert.deepEqual(started(), {
    command: ["printf", "%s", "hi"],
    env: ["A=1"],
    workdir: "/tmp",
    user: "nobody",
    stdin: false,
    tty: true,
    size: { rows: 24, cols: 80 },
    attach: true,
  });
});

test("run feeds stdin given as text or bytes, then ends it", async () => {
  const inputs = ["from text", new TextEncoder().encode("from bytes")];
  for (const [i, stdin] of inputs.entries()) {
    const running = commands.run(["cat"], { stdin });
    const peer = await daemon.peer(i);
    peer.send(1, await stdinOf(peer));
    peer.exit({ code: 0 });
    assert.equal((await running).stdout, ["from text", "from bytes"][i]);
    assert.equal(started(i).stdin, true);
  }
});

test("a command that ends before it reads its input answers its exit", async () => {
  const running = commands.run("true", { stdin: "x".repeat(4 * 1024 * 1024) });
  const peer = await daemon.peer(0);
  // The daemon reads nothing more, so the input backs up and its writes fail once the stream ends.
  peer.socket.pause();
  peer.exit({ code: 0 });
  peer.close();
  peer.socket.end();
  assert.deepEqual(await running, { exitCode: 0, signal: null, lostBytes: 0, stdout: "", stderr: "" });
});

test("a stdin write that fails on a stream cut before the exit rejects run", async () => {
  const running = commands.run("cat", { stdin: "x".repeat(4 * 1024 * 1024) });
  const peer = await daemon.peer(0);
  peer.socket.pause();
  peer.socket.destroy();
  await assert.rejects(running, ConnectionError);
});

test("a stdin write that fails in this client rejects run at once and lets go of the stream", async () => {
  const running = commands.run("cat", { stdin: new Broken(4) });
  const peer = await daemon.peer(0);
  await assert.rejects(running, /local failure/);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
});

test("a command whose exit came with the attach answers it, whatever its input", async () => {
  daemon.upgrade = (peer) => {
    // Written right behind the handshake, so the exit reaches the client in the same read.
    queueMicrotask(() => {
      peer.exit({ code: 0 });
      peer.close();
    });
  };
  assert.deepEqual(await commands.run("true", { stdin: "x" }), { exitCode: 0, signal: null, lostBytes: 0, stdout: "", stderr: "" });
});

test("the output limit keeps the newest bytes, while the callbacks get every chunk", async () => {
  const chunks: string[] = [];
  const running = commands.run("x", { outputLimitBytes: 4, onStdout: (c) => chunks.push(`o${Buffer.from(c)}`), onStderr: (c) => chunks.push(`e${Buffer.from(c)}`) });
  const peer = await daemon.peer(0);
  peer.send(1, "abc");
  peer.send(2, "de");
  peer.send(1, "fg");
  peer.exit({ code: 0 });
  const { stdout, stderr } = await running;
  assert.deepEqual([stdout, stderr], ["fg", "de"]);
  assert.deepEqual(chunks, ["oabc", "ede", "ofg"]);
});

test("a zero output limit keeps nothing", async () => {
  const running = commands.run("x", { outputLimitBytes: 0 });
  const peer = await daemon.peer(0);
  peer.send(1, "abc");
  peer.exit({ code: 0 });
  assert.deepEqual(await running, { exitCode: 0, signal: null, lostBytes: 0, stdout: "", stderr: "" });
});

test("an abort rejects run with the signal's reason and kills nothing", async () => {
  const controller = new AbortController();
  const running = commands.run("sleep 300", { signal: controller.signal });
  await daemon.peer(0);
  controller.abort();
  await assert.rejects(running, (err: unknown) => err instanceof Error && err.name === "AbortError");
  assert.ok(!daemon.requests.some((r) => r.url.pathname.endsWith("/kill")));
});

test("start answers a handle that writes stdin, resizes, kills and waits", async () => {
  const bodies: Array<[string, unknown]> = [];
  for (const verb of ["kill", "resize"]) {
    routes.set(`POST ${execs}/ex_1/${verb}`, (request) => {
      bodies.push([verb, JSON.parse(request.body)]);

      return { status: 204 };
    });
  }
  const command = await commands.start(["cat"], { stdin: true });
  assert.equal(command.id, "ex_1");
  assert.equal(command.sandboxId, "sb_1");
  const peer = await daemon.peer(0);
  await command.writeStdin("in");
  await command.closeStdin();
  assert.equal(await stdinOf(peer), "in");
  await command.resize(40, 120);
  await command.kill();
  peer.send(1, "in");
  peer.exit({ code: 143, signal: 15 });
  assert.deepEqual(await command.wait(), { exitCode: 143, signal: 15, lostBytes: 0, stdout: "in", stderr: "" });
  assert.deepEqual(bodies, [
    ["resize", { rows: 40, cols: 120 }],
    ["kill", {}],
  ]);
});

test("start sends stdin given as text before it answers", async () => {
  const starting = commands.start(["cat"], { stdin: "early" });
  assert.equal(await stdinOf(await daemon.peer(0)), "early");
  await starting;
});

test("start answers the handle of a command that ended before it read its input", async () => {
  const starting = commands.start("true", { stdin: "x".repeat(4 * 1024 * 1024) });
  const peer = await daemon.peer(0);
  peer.socket.pause();
  peer.exit({ code: 0 });
  peer.close();
  peer.socket.end();
  const command = await starting;
  assert.deepEqual(await command.wait(), { exitCode: 0, signal: null, lostBytes: 0, stdout: "", stderr: "" });
});

test("a stdin write that fails on a stream cut before the exit rejects start", async () => {
  const starting = commands.start("cat", { stdin: "x".repeat(4 * 1024 * 1024) });
  const peer = await daemon.peer(0);
  peer.socket.pause();
  peer.socket.destroy();
  await assert.rejects(starting, (err: unknown) => err instanceof ConnectionError && /dropped$/.test(err.message));
});

test("a stdin write that fails in this client rejects start and lets go of the stream", async () => {
  const starting = commands.start("cat", { stdin: new Broken(4) });
  const peer = await daemon.peer(0);
  await assert.rejects(starting, /local failure/);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
});

test("list follows the daemon's pages", async () => {
  routes.set(`GET ${execs}`, (request) => {
    const cursor = request.url.searchParams.get("cursor");
    if (!cursor) {
      return { status: 200, json: { execs: [record()], next: "ex_1" } };
    }

    return { status: 200, json: { execs: [record({ exec: "ex_2", state: "exited", exit_status: { code: 0, signal: 15 } })], next: null } };
  });
  const listed = await commands.list();
  assert.deepEqual(
    listed.map((c) => [c.id, c.state, c.exitCode, c.signal]),
    [
      ["ex_1", "running", null, null],
      ["ex_2", "exited", 0, 15],
    ],
  );
});

test("a page the SDK cannot read is a protocol error", async () => {
  routes.set(`GET ${execs}`, () => ({ status: 200, json: { execs: "none", next: null } }));
  await assert.rejects(commands.list(), ProtocolError);
});

test("get finds a command another client started, and wait attaches for its output", async () => {
  routes.set(`GET ${execs}/ex_1`, () => ({ status: 200, json: record() }));
  const seen: string[] = [];
  const command = await commands.get("ex_1", { onStdout: (c) => seen.push(Buffer.from(c).toString()) });
  const waited = command.wait();
  const peer = await daemon.peer(0);
  peer.send(1, "replayed");
  peer.exit({ code: 0 });
  assert.equal((await waited).stdout, "replayed");
  assert.deepEqual(seen, ["replayed"]);
});

test("get of a command the daemon does not hold throws NotFoundError", async () => {
  await assert.rejects(commands.get("ex_9"), NotFoundError);
});
