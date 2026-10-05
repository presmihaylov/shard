import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { OutputCapture } from "../src/capture.js";
import { CommandNotStartedError, NotFoundError, ProtocolError, ShardConnectionError, UnsupportedError } from "../src/errors.js";
import { Session } from "../src/exec.js";
import { opBinary, opClose } from "../src/frames.js";
import { Transport } from "../src/transport.js";
import { version } from "../src/version.js";
import { execRequest, maxPayload } from "../src/wire.js";
import { FakeDaemon, type Answer, type Request } from "./helpers/daemon.js";

const execPath = "/v0/sandboxes/sb_1/exec/ex_1";

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
let routes: Map<string, (request: Request) => Answer>;

beforeEach(async () => {
  daemon = await FakeDaemon.start();
  transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined });
  routes = new Map([[`POST /v0/sandboxes/sb_1/exec`, () => ({ status: 201, json: record() })]]);
  daemon.route = (request) => routes.get(`${request.method} ${request.url.pathname}`)?.(request);
  daemon.upgrade = () => undefined;
});

afterEach(async () => {
  transport.close();
  await daemon.close();
});

async function start(capture = new OutputCapture(1024), handlers = {}, signal?: AbortSignal): Promise<Session> {
  return Session.start(transport, "sb_1", execRequest("echo hi", { stdin: false }), capture, handlers, signal);
}

async function startReading(): Promise<Session> {
  return Session.start(transport, "sb_1", execRequest("cat", { stdin: true }), new OutputCapture(1024), {});
}

test("a command starts, streams to the capture and every handler, and answers its exit", async () => {
  const capture = new OutputCapture(1024);
  const seen: string[] = [];
  const session = await start(capture, {
    onStdout: (chunk: Uint8Array) => seen.push(`out:${Buffer.from(chunk)}`),
    onStderr: (chunk: Uint8Array) => seen.push(`err:${Buffer.from(chunk)}`),
  });
  const create = daemon.requests[0];
  assert.ok(create);
  assert.deepEqual(JSON.parse(create.body), { command: ["/bin/sh", "-c", "echo hi"], stdin: false, tty: false, attach: true });
  assert.equal(create.headers.authorization, "Bearer test-key");
  assert.equal(create.headers["user-agent"], `useshards-typescript/${version}`);

  const peer = await daemon.peer(0);
  assert.equal(peer.path, execPath);
  peer.send(1, "hello ");
  peer.send(2, "oops");
  peer.send(1, "world");
  peer.exit({ code: 3, signal: 0, lost_bytes: 7 });
  peer.close();

  assert.deepEqual(await session.wait(), { exitCode: 3, signal: null, lostBytes: 7 });
  assert.deepEqual(seen, ["out:hello ", "err:oops", "out:world"]);
  const { stdout, stderr } = capture.output();
  assert.equal(stdout.toString(), "hello world");
  assert.equal(stderr.toString(), "oops");
  const echoed = await peer.next();
  assert.equal(echoed?.opcode, opClose, "the client answers the daemon's close");
  await peer.ended;
  assert.deepEqual(await session.wait(), { exitCode: 3, signal: null, lostBytes: 7 }, "a second wait answers the same exit");
});

test("a signal exit names the signal", async () => {
  const session = await start();
  const peer = await daemon.peer(0);
  peer.exit({ code: 143, signal: 15 });
  assert.deepEqual(await session.wait(), { exitCode: 143, signal: 15, lostBytes: 0 });
});

test("an exit with an error is a command that never ran", async () => {
  const session = await start();
  (await daemon.peer(0)).exit({ code: 127, error: "exec: no such file: /nope" });
  await assert.rejects(session.wait(), (err: unknown) => err instanceof CommandNotStartedError && err.exitCode === 127 && err.reason === "exec: no such file: /nope");
});

test("a failure message is the error a refusal would have been", async () => {
  const session = await start();
  (await daemon.peer(0)).send(5, JSON.stringify({ error: { code: "unsupported", message: "sysbox: exec tty is unsupported" } }));
  await assert.rejects(session.wait(), (err: unknown) => err instanceof UnsupportedError && err.detail === "sysbox: exec tty is unsupported");
});

test("a stream the daemon never sends is refused", async () => {
  const session = await start();
  (await daemon.peer(0)).send(9, "x");
  await assert.rejects(session.wait(), (err: unknown) => err instanceof ProtocolError && /stream 9/.test(err.message));
});

test("a refused attach throws the daemon's error", async () => {
  daemon.upgrade = () => ({ status: 404, json: { error: { code: "not_found", message: "no command ex_1" } } });
  await assert.rejects(start(), (err: unknown) => err instanceof NotFoundError && err.detail === "no command ex_1");
});

test("a stream cut before the exit names the command's state and the bytes lost", async () => {
  routes.set(`GET ${execPath}`, () => ({ status: 200, json: record({ state: "running", lost_bytes: 42 }) }));
  const session = await start();
  (await daemon.peer(0)).socket.destroy();
  await assert.rejects(session.wait(), (err: unknown) => err instanceof ShardConnectionError && /the command is running, with 42 bytes of output lost$/.test(err.message));
});

test("a cut whose record cannot be read is still a cut", async () => {
  const session = await start();
  (await daemon.peer(0)).socket.destroy();
  await assert.rejects(
    session.wait(),
    (err: unknown) => err instanceof ShardConnectionError && /ended without an exit status$/.test(err.message) && err.cause instanceof NotFoundError,
  );
});

test("disconnect leaves the command running, and wait attaches again for the replay and the exit", async () => {
  const capture = new OutputCapture(1024);
  const session = await start(capture);
  const first = await daemon.peer(0);
  first.send(1, "before");
  await until(() => capture.output().stdout.length > 0);
  session.disconnect();
  assert.equal((await first.next())?.opcode, opClose, "the client says goodbye");
  first.close();
  const waited = session.wait();
  const second = await daemon.peer(1);
  second.send(1, "before");
  second.send(1, " after");
  second.exit({ code: 0, lost_bytes: 5 });
  assert.deepEqual(await waited, { exitCode: 0, signal: null, lostBytes: 5 });
  assert.equal(capture.output().stdout.toString(), "before after");
  assert.ok(!daemon.requests.some((r) => r.url.pathname.endsWith("/kill")), "nothing killed the command");
});

test("wait on a command no stream ever held attaches for its output and its exit", async () => {
  const capture = new OutputCapture(1024);
  const session = new Session(transport, "sb_1", "ex_1", capture, {});
  const waited = session.wait();
  const peer = await daemon.peer(0);
  assert.equal(peer.path, execPath);
  peer.send(2, "held");
  peer.exit({ code: 1 });
  assert.deepEqual(await waited, { exitCode: 1, signal: null, lostBytes: 0 });
  assert.equal(capture.output().stderr.toString(), "held");
});

test("a stream that failed is not attached again by wait", async () => {
  const session = await start();
  (await daemon.peer(0)).send(5, JSON.stringify({ error: { code: "not_found", message: "no sandbox sb_1" } }));
  await assert.rejects(session.wait(), NotFoundError);
  await assert.rejects(session.wait(), NotFoundError);
  assert.equal(daemon.peers.length, 1);
});

test("a reattach replays from the daemon's oldest byte, so the capture starts again", async () => {
  const capture = new OutputCapture(1024);
  const seen: string[] = [];
  const session = await start(capture, { onStdout: (chunk: Uint8Array) => seen.push(Buffer.from(chunk).toString()) });
  const first = await daemon.peer(0);
  first.send(1, "one");
  await until(() => seen.length === 1);
  const attached = session.attach();
  assert.equal((await first.next())?.opcode, opClose);
  first.close();
  await attached;
  const second = await daemon.peer(1);
  second.send(1, "one");
  second.send(1, "two");
  second.exit({ code: 0 });
  await session.wait();
  assert.deepEqual(seen, ["one", "one", "two"]);
  assert.equal(capture.output().stdout.toString(), "onetwo");
});

test("stdin goes in order, in pieces the daemon reads, and its close is one message", async () => {
  const session = await startReading();
  const peer = await daemon.peer(0);
  const big = Buffer.alloc(maxPayload + 10, 0x61);
  await Promise.all([session.writeStdin(big), session.writeStdin("tail"), session.closeStdin()]);
  const got: Array<[number, number]> = [];
  for (let i = 0; i < 4; i++) {
    const frame = await peer.next();
    assert.equal(frame?.opcode, opBinary);
    got.push([frame?.payload[0] ?? -1, (frame?.payload.length ?? 0) - 1]);
  }
  assert.deepEqual(got, [
    [0, maxPayload],
    [0, 10],
    [0, 4],
    [4, 0],
  ]);
});

test("stdin to a command no stream holds is refused", async () => {
  const session = await startReading();
  session.disconnect();
  await assert.rejects(session.writeStdin("x"), (err: unknown) => err instanceof ShardConnectionError && /is not attached$/.test(err.message));
});

test("stdin to a command started without stdin or a terminal is refused, as the daemon drops it", async () => {
  const session = await start();
  const peer = await daemon.peer(0);
  await assert.rejects(session.writeStdin("x"), (err: unknown) => err instanceof TypeError && /started without stdin, so it reads no input$/.test(err.message));
  peer.exit({ code: 0 });
  await session.wait();
  assert.notEqual((await peer.next())?.opcode, opBinary, "no stdin reached the daemon");
});

test("stdin to a command on a terminal reaches it without stdin set", async () => {
  const session = await Session.start(transport, "sb_1", execRequest("sh", { stdin: false, tty: { rows: 24, cols: 80 } }), new OutputCapture(1024), {});
  const peer = await daemon.peer(0);
  await session.writeStdin("x");
  const frame = await peer.next();
  assert.deepEqual([...(frame?.payload ?? [])], [0, 0x78]);
});

test("kill, resize and inspect reach their routes", async () => {
  const bodies: string[] = [];
  const noContent = (request: Request): Answer => {
    bodies.push(request.body);

    return { status: 204 };
  };
  routes.set(`POST ${execPath}/kill`, noContent);
  routes.set(`POST ${execPath}/resize`, noContent);
  routes.set(`GET ${execPath}`, () => ({
    status: 200,
    json: record({ state: "exited", exit_status: { code: 137, signal: 9 }, exited_at: "2026-10-04T10:00:05Z" }),
  }));
  const session = await start();
  await session.kill();
  await session.kill("KILL");
  await session.resize({ rows: 40, cols: 100 });
  assert.deepEqual(bodies.map((b) => JSON.parse(b)), [{}, { signal: "KILL" }, { rows: 40, cols: 100 }]);
  assert.deepEqual(await session.inspect(), {
    id: "ex_1",
    sandboxId: "sb_1",
    command: ["/bin/sh", "-c", "echo hi"],
    state: "exited",
    exitCode: 137,
    signal: 9,
    startedAt: new Date("2026-10-04T10:00:00Z"),
    exitedAt: new Date("2026-10-04T10:00:05Z"),
    lostBytes: 0,
  });
});

test("an abort lets go of the stream, leaves the command, and rejects with the caller's reason", async () => {
  const controller = new AbortController();
  const session = await start(undefined, {}, controller.signal);
  const peer = await daemon.peer(0);
  const waited = session.wait(controller.signal);
  const reason = new Error("caller gave up");
  controller.abort(reason);
  await assert.rejects(waited, (err: unknown) => err === reason);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
  assert.ok(!daemon.requests.some((r) => r.url.pathname.endsWith("/kill")), "nothing killed the command");
});

test("an abort of a later wait lets go of a stream another signal opened", async () => {
  const session = await start();
  const peer = await daemon.peer(0);
  const controller = new AbortController();
  const waited = session.wait(controller.signal);
  const reason = new Error("caller gave up");
  controller.abort(reason);
  await assert.rejects(waited, (err: unknown) => err === reason);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
  assert.ok(!daemon.requests.some((r) => r.url.pathname.endsWith("/kill")), "nothing killed the command");
});

test("a command record the SDK cannot read is a protocol error", async () => {
  for (const json of [{ exec: 7 }, record({ state: "paused" }), record({ started_at: "yesterday" }), record({ exit_status: { code: 1 } })]) {
    routes.set(`POST /v0/sandboxes/sb_1/exec`, () => ({ status: 201, json }));
    await assert.rejects(start(), ProtocolError);
  }
});

async function until(ready: () => boolean): Promise<void> {
  for (let i = 0; i < 200 && !ready(); i++) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.ok(ready(), "the condition never held");
}
