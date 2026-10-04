import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { App } from "../src/app.js";
import { ConnectionError, NotFoundError, ProtocolError, ServerError } from "../src/errors.js";
import { opClose } from "../src/frames.js";
import { sandboxInfo } from "../src/records.js";
import { Sandbox } from "../src/sandbox.js";
import { Transport } from "../src/transport.js";
import { FakeDaemon, type Answer, type Request } from "./helpers/daemon.js";
import { sandboxRecord } from "./helpers/records.js";

let daemon: FakeDaemon;
let transport: Transport;
let sandbox: Sandbox;
let routes: Map<string, (request: Request) => Answer>;

beforeEach(async () => {
  daemon = await FakeDaemon.start();
  transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined });
  sandbox = new Sandbox(transport, sandboxInfo(sandboxRecord()));
  routes = new Map();
  daemon.route = (request) => routes.get(`${request.method} ${request.url.pathname}`)?.(request);
  daemon.upgrade = () => undefined;
});

afterEach(async () => {
  transport.close();
  await daemon.close();
});

function sent(method: string, path: string): Request {
  const request = daemon.requests.find((r) => r.method === method && r.url.pathname === path);
  assert.ok(request, `the daemon got ${method} ${path}`);

  return request;
}

async function collect<T>(items: AsyncIterable<T>): Promise<T[]> {
  const all: T[] = [];
  for await (const item of items) {
    all.push(item);
  }

  return all;
}

const egress = { time: "2026-10-04T10:00:00Z", source: "proxy", verdict: "allow", host: "example.com", port: 443, rule: "r1" };

test("each lifecycle verb keeps the handle's info current", async () => {
  routes.set("POST /v0/sandboxes/sb_1/stop", () => ({ status: 200, json: sandboxRecord({ state: "stopped", stopped_reason: "stopped" }) }));
  routes.set("POST /v0/sandboxes/sb_1/start", () => ({ status: 200, json: sandboxRecord() }));
  routes.set("GET /v0/sandboxes/sb_1", () => ({ status: 200, json: sandboxRecord({ state: "paused" }) }));
  await sandbox.stop();
  assert.equal(sandbox.info.state, "stopped");
  assert.equal(sandbox.info.stoppedReason, "stopped");
  await sandbox.start();
  assert.equal(sandbox.info.state, "running");
  assert.equal((await sandbox.inspect()).state, "paused");
  assert.equal(sandbox.info.state, "paused");
});

test("a fork answers a new handle, and the source runs on", async () => {
  routes.set("POST /v0/sandboxes/sb_1/fork", () => ({ status: 201, json: sandboxRecord({ id: "sb_2", name: "copy" }) }));
  const forked = await sandbox.fork({ name: "copy" });
  assert.equal(forked.id, "sb_2");
  assert.equal(sandbox.id, "sb_1");
  assert.deepEqual(JSON.parse(sent("POST", "/v0/sandboxes/sb_1/fork").body), { name: "copy" });
});

test("a forced remove says so", async () => {
  routes.set("DELETE /v0/sandboxes/sb_1", () => ({ status: 204 }));
  await sandbox.remove({ force: true });
  assert.equal(sent("DELETE", "/v0/sandboxes/sb_1").url.searchParams.get("force"), "true");
});

test("logs answer the app's output as text", async () => {
  routes.set("GET /v0/sandboxes/sb_1/logs", () => ({ status: 200, raw: "hello\nworld\n", headers: { "Content-Type": "text/plain" } }));
  assert.equal(await sandbox.logs(), "hello\nworld\n");
});

test("a log follow yields each chunk and ends with the sandbox", async () => {
  const chunks = collect(sandbox.followLogs());
  const peer = await daemon.peer(0);
  assert.equal(peer.path, "/v0/sandboxes/sb_1/logs");
  peer.send(1, "hello ");
  peer.send(1, "world");
  peer.send(3, JSON.stringify({ reason: "stopped" }));
  peer.close();
  assert.deepEqual(
    (await chunks).map((chunk) => Buffer.from(chunk).toString()),
    ["hello ", "world"],
  );
  assert.equal(daemon.requests[0]?.url.searchParams.get("follow"), "true");
});

test("a failure on a log follow throws the daemon's error", async () => {
  const chunks = collect(sandbox.followLogs());
  const peer = await daemon.peer(0);
  peer.send(5, JSON.stringify({ error: { code: "not_found", message: "no such sandbox" } }));
  await assert.rejects(chunks, NotFoundError);
});

test("a follow the daemon closes with an error throws it, and a dropped one throws ConnectionError", async () => {
  const failed = collect(sandbox.followLogs());
  (await daemon.peer(0)).close(1011, "tail broke");
  await assert.rejects(failed, (err: unknown) => err instanceof ServerError && err.message.includes("tail broke"));
  const dropped = collect(sandbox.followLogs());
  (await daemon.peer(1)).socket.destroy();
  await assert.rejects(dropped, ConnectionError);
});

test("a break out of a follow lets go of the stream", async () => {
  const follow = sandbox.followLogs();
  const first = follow.next();
  const peer = await daemon.peer(0);
  peer.send(1, "once");
  assert.equal(Buffer.from((await first).value ?? []).toString(), "once");
  await follow.return(undefined);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
});

test("an abort ends a follow with the caller's reason", async () => {
  const controller = new AbortController();
  const follow = sandbox.followLogs({ signal: controller.signal });
  const first = follow.next();
  const peer = await daemon.peer(0);
  peer.send(1, "once");
  await first;
  const pending = follow.next();
  const reason = new Error("caller gave up");
  controller.abort(reason);
  await assert.rejects(pending, (err: unknown) => err === reason);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
});

test("a network log answers its records, and none as null", async () => {
  routes.set("GET /v0/sandboxes/sb_1/egress-log", () => ({ status: 200, json: [egress] }));
  const [record] = await sandbox.networkLogs();
  assert.equal(record?.host, "example.com");
  assert.equal(record?.port, 443);
  routes.set("GET /v0/sandboxes/sb_1/egress-log", () => ({ status: 200, json: null }));
  assert.deepEqual(await sandbox.networkLogs(), []);
});

test("a network log follow yields each record until a normal close", async () => {
  const records = collect(sandbox.followNetworkLogs());
  const peer = await daemon.peer(0);
  assert.equal(peer.path, "/v0/sandboxes/sb_1/egress-log");
  peer.text(JSON.stringify(egress));
  peer.text(JSON.stringify({ ...egress, verdict: "deny", source: "dns", port: 0, reason: "no rule" }));
  peer.close();
  assert.deepEqual(
    (await records).map((each) => [each.verdict, each.port]),
    [
      ["allow", 443],
      ["deny", null],
    ],
  );
});

test("a network log follow refuses what is not a JSON record", async () => {
  const binary = collect(sandbox.followNetworkLogs());
  (await daemon.peer(0)).send(1, "raw");
  await assert.rejects(binary, ProtocolError);
  const garbled = collect(sandbox.followNetworkLogs());
  (await daemon.peer(1)).text("{");
  await assert.rejects(garbled, ProtocolError);
});

test("an app waits on attach and stops by TERM, or KILL with force", async () => {
  const app = new App(transport, sandbox);
  routes.set("GET /v0/sandboxes/sb_1/attach", () => ({ status: 200, json: { code: 0, signal: 15, restarts: 2 } }));
  routes.set("POST /v0/sandboxes/sb_1/app/stop", () => ({ status: 204 }));
  assert.deepEqual(await app.wait(), { exitCode: 0, signal: 15, restarts: 2 });
  await app.stop();
  await app.stop({ force: true });
  const stops = daemon.requests.filter((r) => r.url.pathname === "/v0/sandboxes/sb_1/app/stop").map((r) => r.body);
  assert.deepEqual(stops, ["", JSON.stringify({ force: true })]);
});

test("an app whose sandbox answers no app is a protocol error", async () => {
  routes.set("GET /v0/sandboxes/sb_1", () => ({ status: 200, json: sandboxRecord() }));
  await assert.rejects(new App(transport, sandbox).inspect(), ProtocolError);
  routes.set("GET /v0/sandboxes/sb_1", () => ({ status: 200, json: sandboxRecord({ command: ["sleep", "9"] }) }));
  assert.deepEqual((await new App(transport, sandbox).inspect()).command, ["sleep", "9"]);
});
