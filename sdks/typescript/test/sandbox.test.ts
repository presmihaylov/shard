import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { APIError, CommandNotStartedError, ConflictError, NotFoundError, PermissionDeniedError, ProtocolError, ServerError, ShardConnectionError, UnsupportedError } from "../src/errors.js";
import { opClose } from "../src/frames.js";
import { Process } from "../src/process.js";
import { processInfo, sandboxInfo } from "../src/records.js";
import { Sandbox } from "../src/sandbox.js";
import { Transport } from "../src/transport.js";
import { FakeDaemon, type Answer, type Request } from "./helpers/daemon.js";
import { portRecord, processRecord, sandboxRecord } from "./helpers/records.js";

let daemon: FakeDaemon;
let transport: Transport;
let sandbox: Sandbox;
let web: Process;
let routes: Map<string, (request: Request) => Answer>;

beforeEach(async () => {
  daemon = await FakeDaemon.start();
  transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined });
  sandbox = new Sandbox(transport, sandboxInfo(sandboxRecord()));
  web = new Process(transport, "sb_1", processInfo(processRecord()));
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

test("info carries the memory kills and the start again the daemon owes", async () => {
  const oom = { kills: 2, killed_at: "2026-10-07T12:00:00Z", restart_at: "2026-10-07T12:00:10Z" };
  routes.set("GET /v0/sandboxes/sb_1", () => ({ status: 200, json: sandboxRecord({ state: "stopped", stopped_reason: "ran out of memory and the host ended it", oom }) }));
  const info = await sandbox.inspect();
  assert.deepEqual(info.oom, { kills: 2, killedAt: new Date(oom.killed_at), restartAt: new Date(oom.restart_at) });
  routes.set("GET /v0/sandboxes/sb_1", () => ({ status: 200, json: sandboxRecord() }));
  assert.equal((await sandbox.inspect()).oom, null);
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

test("a port add puts the guest port on the host port's route and answers the forward", async () => {
  routes.set("PUT /v0/sandboxes/sb_1/ports/9000", () => ({ status: 200, json: portRecord({ public: true, address: "0.0.0.0" }) }));
  const added = await sandbox.ports.add(9000, 8000, { public: true });
  assert.deepEqual(JSON.parse(sent("PUT", "/v0/sandboxes/sb_1/ports/9000").body), { guest_port: 8000, public: true });
  assert.deepEqual(added, {
    sandbox: "sb_1",
    sandboxName: "web",
    hostPort: 9000,
    guestPort: 8000,
    public: true,
    address: "0.0.0.0",
    listening: true,
    error: null,
    reachableOn: [{ interface: "lo", address: "127.0.0.1" }],
  });
  routes.set("PUT /v0/sandboxes/sb_1/ports/9001", () => ({ status: 200, json: portRecord({ host_port: 9001 }) }));
  await sandbox.ports.add(9001, 8000);
  assert.deepEqual(JSON.parse(sent("PUT", "/v0/sandboxes/sb_1/ports/9001").body), { guest_port: 8000 });
});

test("a port list reads every page of the sandbox's forwards, and a remove names the host port", async () => {
  routes.set("GET /v0/sandboxes/sb_1/ports", (request) => {
    if (request.url.searchParams.get("cursor") === "9000") {
      return { status: 200, json: { ports: [portRecord({ host_port: 9001, listening: false, reachable_on: [], error: "connection refused" })], next: null } };
    }

    return { status: 200, json: { ports: [portRecord()], next: "9000" } };
  });
  routes.set("DELETE /v0/sandboxes/sb_1/ports/9000", () => ({ status: 204 }));
  const listed = await sandbox.ports.list();
  assert.deepEqual(
    listed.map((each) => [each.hostPort, each.listening, each.error]),
    [
      [9000, true, null],
      [9001, false, "connection refused"],
    ],
  );
  await sandbox.ports.remove(9000);
  sent("DELETE", "/v0/sandboxes/sb_1/ports/9000");
});

test("a port refusal throws the error of its code", async () => {
  const refusals: [number, string, new (...args: never[]) => Error][] = [
    [404, "not_found", NotFoundError],
    [409, "in_use", ConflictError],
    [409, "unsupported", UnsupportedError],
    [403, "forbidden", PermissionDeniedError],
  ];
  for (const [status, code, kind] of refusals) {
    routes.set("PUT /v0/sandboxes/sb_1/ports/9000", () => ({ status, json: { error: { code, message: `refused as ${code}`, holders: ["sb_2"] } } }));
    await assert.rejects(sandbox.ports.add(9000, 8000), (err: unknown) => err instanceof kind && err.message.includes(`refused as ${code}`));
  }
  routes.set("DELETE /v0/sandboxes/sb_1/ports/9000", () => ({ status: 404, json: { error: { code: "not_found", message: "sandbox web forwards no host port 9000" } } }));
  await assert.rejects(sandbox.ports.remove(9000), NotFoundError);
});

test("logs answer the process's output as text", async () => {
  routes.set("GET /v0/sandboxes/sb_1/processes/web/logs", () => ({ status: 200, raw: "hello\nworld\n", headers: { "Content-Type": "text/plain" } }));
  assert.equal(await web.logs(), "hello\nworld\n");
});

test("a log follow yields each chunk and ends with the process", async () => {
  const chunks = collect(web.followLogs());
  const peer = await daemon.peer(0);
  assert.equal(peer.path, "/v0/sandboxes/sb_1/processes/web/logs");
  peer.send(1, "hello ");
  peer.send(1, "world");
  peer.send(3, JSON.stringify({ reason: "ended" }));
  peer.close();
  assert.deepEqual(
    (await chunks).map((chunk) => Buffer.from(chunk).toString()),
    ["hello ", "world"],
  );
  assert.equal(daemon.requests[0]?.url.searchParams.get("follow"), "true");
});

test("a failure on a log follow throws the daemon's error", async () => {
  const chunks = collect(web.followLogs());
  const peer = await daemon.peer(0);
  peer.send(5, JSON.stringify({ error: { code: "not_found", message: "no such sandbox" } }));
  await assert.rejects(chunks, NotFoundError);
});

test("a follow the daemon closes with an error throws it, and a dropped one throws ShardConnectionError", async () => {
  const failed = collect(web.followLogs());
  (await daemon.peer(0)).close(1011, "tail broke");
  await assert.rejects(failed, (err: unknown) => err instanceof ServerError && err.message.includes("tail broke"));
  const dropped = collect(web.followLogs());
  (await daemon.peer(1)).socket.destroy();
  await assert.rejects(dropped, ShardConnectionError);
});

test("a break out of a follow lets go of the stream", async () => {
  const follow = web.followLogs();
  const first = follow.next();
  const peer = await daemon.peer(0);
  peer.send(1, "once");
  assert.equal(Buffer.from((await first).value ?? []).toString(), "once");
  await follow.return(undefined);
  assert.equal((await peer.next())?.opcode, opClose, "the client says goodbye");
});

test("an abort ends a follow with the caller's reason", async () => {
  const controller = new AbortController();
  const follow = web.followLogs({ signal: controller.signal });
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

test("an egress log answers its records, and none as null", async () => {
  routes.set("GET /v0/sandboxes/sb_1/egress-log", () => ({ status: 200, json: [egress] }));
  const [record] = await sandbox.egressLog();
  assert.equal(record?.host, "example.com");
  assert.equal(record?.port, 443);
  routes.set("GET /v0/sandboxes/sb_1/egress-log", () => ({ status: 200, json: null }));
  assert.deepEqual(await sandbox.egressLog(), []);
});

test("an egress log follow yields each record until a normal close", async () => {
  const records = collect(sandbox.followEgressLog());
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

test("an egress log follow refuses what is not a JSON record", async () => {
  const binary = collect(sandbox.followEgressLog());
  (await daemon.peer(0)).send(1, "raw");
  await assert.rejects(binary, ProtocolError);
  const garbled = collect(sandbox.followEgressLog());
  (await daemon.peer(1)).text("{");
  await assert.rejects(garbled, ProtocolError);
});

test("run starts a string under a shell, and answers a handle by the name the daemon gave", async () => {
  routes.set("POST /v0/sandboxes/sb_1/processes", () => ({ status: 201, json: processRecord() }));
  const run = await sandbox.run("serve", { env: { MODE: "test" }, restart: { policy: "on-failure", retries: 3 } });
  assert.equal(run.name, "web");
  assert.equal(run.info.status.state, "running");
  assert.deepEqual(JSON.parse(sent("POST", "/v0/sandboxes/sb_1/processes").body), {
    command: ["/bin/sh", "-c", "serve"],
    env: ["MODE=test"],
    restart: { policy: "on-failure", retries: 3 },
  });
  await assert.rejects(sandbox.run([]), TypeError);
});

test("a run of a name that still runs is a conflict", async () => {
  const refusal = { error: { code: "name_taken", message: "sandbox web is running: its process web still runs" } };
  routes.set("POST /v0/sandboxes/sb_1/processes", () => ({ status: 409, json: refusal }));
  await assert.rejects(sandbox.run(["serve"], { name: "web" }), (err) => err instanceof ConflictError && err.code === "name_taken");
});

test("a run whose command never started throws CommandNotStartedError, and one with no exit code stays an APIError", async () => {
  const refusal = { error: { code: "command_not_started", message: "exec: no such file", exit_code: 127 } };
  routes.set("POST /v0/sandboxes/sb_1/processes", () => ({ status: 422, json: refusal }));
  const err = await sandbox.run(["/nope"]).then(
    () => assert.fail("the run started"),
    (caught: unknown) => caught,
  );
  assert.ok(err instanceof CommandNotStartedError);
  assert.equal(err.exitCode, 127);
  assert.equal(err.reason, "exec: no such file");
  routes.set("POST /v0/sandboxes/sb_1/processes", () => ({ status: 422, json: { error: { code: "command_not_started", message: "gone" } } }));
  await assert.rejects(sandbox.run(["/nope"]), (caught) => caught instanceof APIError && caught.status === 422);
});

test("list answers every process in run order, and get a handle to one", async () => {
  routes.set("GET /v0/sandboxes/sb_1/processes", () => ({ status: 200, json: { processes: [processRecord(), processRecord({ name: "worker" })] } }));
  routes.set("GET /v0/sandboxes/sb_1/processes/worker", () => ({ status: 200, json: processRecord({ name: "worker" }) }));
  assert.deepEqual(
    (await sandbox.processes.list()).map((each) => each.name),
    ["web", "worker"],
  );
  assert.equal((await sandbox.processes.get("worker")).name, "worker");
});

test("a process waits on attach and kills by TERM, or KILL with force, keeping its info current", async () => {
  const ended = processRecord({ status: { state: "exited", restarts: 2, exit: { code: 1, signal: 0 } } });
  const killed = processRecord({ killed: true, status: { state: "killed", restarts: 0, exit: { code: 0, signal: 15 } } });
  routes.set("GET /v0/sandboxes/sb_1/processes/web/attach", () => ({ status: 200, json: ended }));
  routes.set("POST /v0/sandboxes/sb_1/processes/web/kill", () => ({ status: 200, json: killed }));
  assert.deepEqual((await web.wait()).status.exit, { exitCode: 1, signal: null });
  assert.equal(web.info.status.state, "exited");
  await web.kill();
  await web.kill({ force: true });
  assert.equal(web.info.status.state, "killed");
  assert.equal(web.info.killed, true);
  const kills = daemon.requests.filter((r) => r.url.pathname === "/v0/sandboxes/sb_1/processes/web/kill").map((r) => r.body);
  assert.deepEqual(kills, ["", JSON.stringify({ force: true })]);
});

test("a process the daemon answers malformed is a protocol error", async () => {
  routes.set("GET /v0/sandboxes/sb_1/processes/web", () => ({ status: 200, json: processRecord({ status: { state: "running" } }) }));
  await assert.rejects(web.inspect(), ProtocolError);
});
