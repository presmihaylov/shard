import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { ConnectionError, ProtocolError, ServerError } from "../src/errors.js";
import { Transport } from "../src/transport.js";
import { FakeDaemon } from "./helpers/daemon.js";
import { certificate } from "./helpers/tls.js";

const opened: Array<{ daemon: FakeDaemon; transport: Transport }> = [];

afterEach(async () => {
  for (const { daemon, transport } of opened.splice(0)) {
    transport.close();
    await daemon.close();
  }
});

async function open(timeoutMs?: number): Promise<{ daemon: FakeDaemon; transport: Transport }> {
  const daemon = await FakeDaemon.start();
  const transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined }, timeoutMs);
  opened.push({ daemon, transport });

  return { daemon, transport };
}

test("a call sends its JSON and its query, and answers the daemon's JSON", async () => {
  const { daemon, transport } = await open();
  daemon.route = (request) => ({ status: 200, json: { method: request.method, query: request.url.search, body: JSON.parse(request.body) } });
  const answer = await transport.call("POST", "/v0/sandboxes", { json: { image: "alpine" }, query: { a: "1" } });
  assert.deepEqual(answer, { method: "POST", query: "?a=1", body: { image: "alpine" } });
  const request = daemon.requests[0];
  assert.ok(request);
  assert.equal(request.headers["content-type"], "application/json");
});

test("an empty body answers undefined", async () => {
  const { daemon, transport } = await open();
  daemon.route = () => ({ status: 204 });
  assert.equal(await transport.call("DELETE", "/v0/sandboxes/sb_1"), undefined);
});

test("a body that is not JSON is a protocol error, and a refusal is the daemon's error", async () => {
  const { daemon, transport } = await open();
  daemon.route = (request) => (request.method === "GET" ? { status: 200, raw: "<html>" } : { status: 502, raw: "bad gateway" });
  await assert.rejects(transport.call("GET", "/v0/sandboxes"), (err: unknown) => err instanceof ProtocolError && /not JSON$/.test(err.message));
  await assert.rejects(transport.call("POST", "/v0/sandboxes"), (err: unknown) => err instanceof ServerError && err.detail === "bad gateway");
});

test("a daemon that does not answer in time is a connection error, and timeoutMs 0 waits", async () => {
  const { daemon, transport } = await open(100);
  daemon.route = async (request) => {
    await new Promise((resolve) => setTimeout(resolve, 300));

    return { status: 200, json: { waited: request.url.pathname } };
  };
  await assert.rejects(transport.call("GET", "/v0/slow"), (err: unknown) => err instanceof ConnectionError && /did not answer in time$/.test(err.message));
  assert.deepEqual(await transport.call("GET", "/v0/wait", { timeoutMs: 0 }), { waited: "/v0/wait" });
});

test("an abort rejects with the caller's reason", async () => {
  const { daemon, transport } = await open();
  daemon.route = () => new Promise(() => undefined);
  const controller = new AbortController();
  const reason = new Error("caller gave up");
  const called = transport.call("GET", "/v0/never", { signal: controller.signal });
  controller.abort(reason);
  await assert.rejects(called, (err: unknown) => err === reason);
});

test("a daemon that is not there is a connection error that names the call", async () => {
  const { daemon, transport } = await open();
  await daemon.close();
  opened.length = 0;
  await assert.rejects(transport.call("GET", "/v0/daemon"), (err: unknown) => err instanceof ConnectionError && err.message.startsWith("GET /v0/daemon: "));
  transport.close();
});

test("https trusts the CA it is given", async () => {
  const tls = certificate();
  const daemon = await FakeDaemon.start(tls);
  daemon.route = () => ({ status: 200, json: { ok: true } });
  const trusted = new Transport({ baseUrl: `https://localhost:${daemon.port}`, apiKey: "test-key", ca: tls.cert });
  const untrusted = new Transport({ baseUrl: `https://localhost:${daemon.port}`, apiKey: "test-key", ca: undefined });
  try {
    assert.deepEqual(await trusted.call("GET", "/v0/sandboxes"), { ok: true });
    await assert.rejects(untrusted.call("GET", "/v0/sandboxes"), ConnectionError);
  } finally {
    trusted.close();
    untrusted.close();
    await daemon.close();
  }
});
