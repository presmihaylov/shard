import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { inspect } from "node:util";
import { after, afterEach, before, beforeEach, test } from "node:test";
import { APIError, CommandNotStartedError, ProtocolError } from "../src/errors.js";
import { Shard } from "../src/shard.js";
import { FakeDaemon, type Answer, type Request } from "./helpers/daemon.js";
import { portRecord, sandboxRecord } from "./helpers/records.js";
import { certificate } from "./helpers/tls.js";

let dir: string;
let caFile: string;
let daemon: FakeDaemon;
let shard: Shard;
let routes: Map<string, (request: Request) => Answer>;

// The fake daemon serves TLS under a CA the test trusts, so the calls ride https as a real remote does.
before(() => {
  dir = mkdtempSync(join(tmpdir(), "useshards-shard-"));
  caFile = join(dir, "ca.pem");
  writeFileSync(caFile, certificate().cert);
});

after(() => rmSync(dir, { recursive: true, force: true }));

beforeEach(async () => {
  daemon = await FakeDaemon.start(certificate());
  shard = new Shard({ remote: `https://localhost:${daemon.port}`, apiKey: "test-key", caFile });
  routes = new Map([["POST /v0/sandboxes", () => ({ status: 201, json: sandboxRecord() })]]);
  daemon.route = (request) => routes.get(`${request.method} ${request.url.pathname}`)?.(request);
});

afterEach(async () => {
  shard.close();
  await daemon.close();
});

function sent(method: string, path: string, index = 0): Request {
  const request = daemon.requests.filter((r) => r.method === method && r.url.pathname === path)[index];
  assert.ok(request, `the daemon got ${method} ${path}`);

  return request;
}

test("create waits for the sandbox to settle and runs no command", async () => {
  const sandbox = await shard.create({ image: "alpine", name: "web", env: { A: "1" }, memoryMiB: 256 });
  assert.equal(sandbox.id, "sb_1");
  assert.equal(sandbox.info.state, "running");
  const request = sent("POST", "/v0/sandboxes");
  assert.equal(request.url.searchParams.get("wait"), "true");
  assert.deepEqual(JSON.parse(request.body), {
    image: "alpine",
    name: "web",
    env: ["A=1"],
    resources: { memory_mib: 256, vcpus: 0, disk_mib: 0 },
  });
});

test("a create forwards its ports, private unless it says public", async () => {
  await shard.create({ image: "alpine", ports: [{ hostPort: 9000, guestPort: 8000 }, { hostPort: 9443, guestPort: 443, public: true }] });
  assert.deepEqual(JSON.parse(sent("POST", "/v0/sandboxes").body).ports, [
    { host_port: 9000, guest_port: 8000 },
    { host_port: 9443, guest_port: 443, public: true },
  ]);
  await shard.create({ image: "alpine", ports: [] });
  assert.equal(JSON.parse(sent("POST", "/v0/sandboxes", 1).body).ports, undefined);
});

test("ports reads every page of every sandbox's forwards", async () => {
  routes.set("GET /v0/ports", (request) => {
    if (request.url.searchParams.get("cursor") === "9000") {
      return { status: 200, json: { ports: [portRecord({ sandbox: "sb_2", sandbox_name: undefined, host_port: 9100 })], next: null } };
    }

    return { status: 200, json: { ports: [portRecord()], next: "9000" } };
  });
  const listed = await shard.ports();
  assert.deepEqual(
    listed.map((each) => [each.sandbox, each.sandboxName, each.hostPort]),
    [
      ["sb_1", "web", 9000],
      ["sb_2", null, 9100],
    ],
  );
  assert.equal(sent("GET", "/v0/ports", 1).url.searchParams.get("cursor"), "9000");
});

test("a create from a snapshot leaves the memory to the snapshot", async () => {
  await shard.create({ snapshot: "snap" });
  assert.deepEqual(JSON.parse(sent("POST", "/v0/sandboxes").body), { snapshot: "snap", resources: { vcpus: 0, disk_mib: 0 } });
});

test("a create names an image or a snapshot, exactly one", async () => {
  await assert.rejects(shard.create({}), TypeError);
  await assert.rejects(shard.create({ image: "alpine", snapshot: "snap" }), TypeError);
  assert.equal(daemon.requests.length, 0);
});

test("a create whose sandbox failed answers it, failed", async () => {
  routes.set("POST /v0/sandboxes", () => ({ status: 201, json: sandboxRecord({ state: "failed", failed_reason: "pull failed" }) }));
  const sandbox = await shard.create({ image: "alpine" });
  assert.equal(sandbox.info.state, "failed");
  assert.equal(sandbox.info.failedReason, "pull failed");
});

test("run starts a string under a shell, with its restart policy", async () => {
  const app = await shard.run("alpine", "echo hi", { restart: { policy: "on-failure", retries: 3 } });
  assert.equal(app.sandbox.id, "sb_1");
  const body = JSON.parse(sent("POST", "/v0/sandboxes").body);
  assert.deepEqual(body.command, ["/bin/sh", "-c", "echo hi"]);
  assert.deepEqual(body.restart, { policy: "on-failure", retries: 3, backoff: 0 });
  await assert.rejects(shard.run("alpine", []), TypeError);
});

test("a run whose command never started throws CommandNotStartedError", async () => {
  const refusal = { error: { code: "command_not_started", message: "exec: no such file", exit_code: 127 } };
  routes.set("POST /v0/sandboxes", () => ({ status: 422, json: refusal }));
  const err = await shard.run("alpine", ["/nope"]).then(
    () => assert.fail("the run started"),
    (caught: unknown) => caught,
  );
  assert.ok(err instanceof CommandNotStartedError);
  assert.equal(err.exitCode, 127);
  assert.equal(err.reason, "exec: no such file");
});

test("a command_not_started with no exit code stays an APIError", async () => {
  routes.set("POST /v0/sandboxes", () => ({ status: 422, json: { error: { code: "command_not_started", message: "gone" } } }));
  await assert.rejects(shard.run("alpine", ["/nope"]), (err) => err instanceof APIError && err.status === 422);
});

test("list reads every page", async () => {
  routes.set("GET /v0/sandboxes", (request) => {
    if (request.url.searchParams.get("cursor") === "c2") {
      return { status: 200, json: { sandboxes: [sandboxRecord({ id: "sb_2" })], next: null, warnings: ["second unreadable entry"] } };
    }

    return { status: 200, json: { sandboxes: [sandboxRecord()], next: "c2", warnings: ["first unreadable entry"] } };
  });
  const listed = await shard.list({ all: true });
  assert.deepEqual(
    listed.sandboxes.map((each) => each.id),
    ["sb_1", "sb_2"],
  );
  assert.deepEqual(listed.warnings, ["first unreadable entry", "second unreadable entry"]);
  assert.equal(sent("GET", "/v0/sandboxes", 1).url.searchParams.get("all"), "true");
});

test("secret list keeps warnings from empty and readable pages", async () => {
  routes.set("GET /v0/secrets", (request) => {
    const cursor = request.url.searchParams.get("cursor");
    if (cursor === "c3") {
      return { status: 200, json: { secrets: [], next: null } };
    }
    if (cursor === "c2") {
      return { status: 200, json: {
        secrets: [{ name: "token", destinations: [], placeholder: "ph_1", updated_at: "2026-10-04T10:00:00Z" }],
        next: "c3", warnings: ["second unreadable secret"],
      } };
    }
    return { status: 200, json: { secrets: [], next: "c2", warnings: ["first unreadable secret"] } };
  });
  const listed = await shard.secrets.list();
  assert.deepEqual(listed.secrets.map((each) => each.name), ["token"]);
  assert.deepEqual(listed.warnings, ["first unreadable secret", "second unreadable secret"]);
});

test("partial lists keep each warning once in first-seen order", async () => {
  for (const key of ["sandboxes", "secrets"] as const) {
    routes.set(`GET /v0/${key}`, (request) => {
      if (request.url.searchParams.has("cursor")) {
        return { status: 200, json: { [key]: [], next: null, warnings: ["first", "third", "First"] } };
      }
      return { status: 200, json: { [key]: [], next: "c2", warnings: ["second", "first", "second"] } };
    });
    const list = key === "sandboxes" ? () => shard.list() : () => shard.secrets.list();
    assert.deepEqual((await list()).warnings, ["second", "first", "third", "First"]);
    assert.deepEqual((await list()).warnings, ["second", "first", "third", "First"]);
  }
});

test("a secret removal keeps the holders on its conflict", async () => {
  routes.set("DELETE /v0/secrets/token", () => ({ status: 409, json: {
    error: { code: "in_use", message: "the secret has a grant", holders: ["sb_1", "sb_2"] },
  } }));
  await assert.rejects(shard.secrets.remove("token"), (err) => {
    assert.ok(err instanceof APIError);
    assert.equal(err.status, 409);
    assert.equal(err.code, "in_use");
    assert.deepEqual(err.holders, ["sb_1", "sb_2"]);
    return true;
  });
});

test("an http remote warns once per client, in the CLI's words, and answers as https does", async () => {
  const plain = await FakeDaemon.start();
  plain.route = () => ({ status: 200, json: { version: "0.9.0", api_version: "v0" } });
  const warnings: Error[] = [];
  const listen = (warning: Error): void => {
    warnings.push(warning);
  };
  process.on("warning", listen);
  const clients = [new Shard({ remote: plain.url, apiKey: "test-key" }), new Shard({ remote: plain.url, apiKey: "test-key" })];
  try {
    for (const client of clients) {
      assert.deepEqual(await client.version(), { version: "0.9.0", apiVersion: "v0" });
      await client.version();
    }
    await new Promise((resolve) => setImmediate(resolve));
    const cli = "Warning: HTTP does not encrypt this connection. Use it only on localhost or through a trusted encrypted network.";
    assert.deepEqual(
      warnings.map((warning) => `${warning.name}: ${warning.message}`),
      [cli, cli],
    );
  } finally {
    process.off("warning", listen);
    clients.forEach((client) => client.close());
    await plain.close();
  }
});

test("printing a client, a sandbox or an attached command never shows the API key", async () => {
  const exec = {
    exec: "ex_1",
    sandbox: "sb_1",
    command: ["sleep", "9"],
    state: "running",
    exit_status: null,
    started_at: "2026-10-04T10:00:00Z",
    exited_at: null,
    truncated: false,
    lost_bytes: 0,
  };
  routes.set("POST /v0/sandboxes/sb_1/exec", () => ({ status: 201, json: exec }));
  daemon.upgrade = () => undefined;
  const sandbox = await shard.create({ image: "alpine" });
  const command = await sandbox.exec("sleep 9", { background: true });
  const peer = await daemon.peer(0);
  for (const handle of [shard, sandbox, command]) {
    assert.doesNotMatch(inspect(handle, { depth: Infinity, showHidden: true }), /test-key/);
    assert.doesNotMatch(JSON.stringify(handle), /test-key/);
  }
  peer.exit({ code: 0, signal: 0, lost_bytes: 0 });
  peer.close();
  await command.wait();
});

test("version and capabilities read the daemon's records", async () => {
  routes.set("GET /v0/version", () => ({ status: 200, json: { version: "0.9.0", api_version: "v0" } }));
  const verbs = { create: true, start: true, stop: true, remove: true, pause: false, resume: false, fork: false, snapshot: true, port: true };
  routes.set("GET /v0/capabilities", () => ({ status: 200, json: verbs }));
  assert.deepEqual(await shard.version(), { version: "0.9.0", apiVersion: "v0" });
  assert.deepEqual(await shard.capabilities(), verbs);
});

test("a capability that is not a boolean is a protocol error", async () => {
  const verbs = { create: true, start: true, stop: true, remove: true, pause: true, resume: true, fork: true, snapshot: "yes" };
  routes.set("GET /v0/capabilities", () => ({ status: 200, json: verbs }));
  await assert.rejects(shard.capabilities(), ProtocolError);
});

test("a policy attach keeps the handle's info current", async () => {
  const sandbox = await shard.create({ image: "alpine" });
  routes.set("PUT /v0/sandboxes/sb_1/policy", () => ({ status: 200, json: sandboxRecord({ policy: "web" }) }));
  routes.set("DELETE /v0/sandboxes/sb_1/policy", () => ({ status: 200, json: sandboxRecord() }));
  await shard.policies.attach(sandbox, "web");
  assert.equal(sandbox.info.policy, "web");
  assert.deepEqual(JSON.parse(sent("PUT", "/v0/sandboxes/sb_1/policy").body), { policy: "web" });
  assert.equal((await shard.policies.detach("sb_1")).policy, null);
  assert.equal(sandbox.info.policy, "web", "a detach by id leaves a handle as it was");
});

test("a policy set sends the rules as written", async () => {
  routes.set("PUT /v0/policies/web", () => ({
    status: 200,
    json: { name: "web", rules: [{ action: "allow", destination: { kind: "domain", value: "example.com" } }], holders: [], dns: "closed" },
  }));
  const set = await shard.policies.set("web", [{ action: "allow", rule: "example.com" }]);
  assert.deepEqual(JSON.parse(sent("PUT", "/v0/policies/web").body), { rules: [{ action: "allow", rule: "example.com" }] });
  assert.deepEqual(set.rules, [{ action: "allow", rule: "example.com" }]);
});

test("secrets set, remove with force, grant and ungrant", async () => {
  routes.set("PUT /v0/secrets/token", () => ({
    status: 200,
    json: { name: "token", destinations: ["api.example.com"], placeholder: "ph_1", updated_at: "2026-10-04T10:00:00Z" },
  }));
  routes.set("DELETE /v0/secrets/token", () => ({ status: 204 }));
  routes.set("POST /v0/sandboxes/sb_1/secrets/token", () => ({ status: 200, json: sandboxRecord({ secrets: ["token"] }) }));
  routes.set("DELETE /v0/sandboxes/sb_1/secrets/token", () => ({ status: 200, json: sandboxRecord() }));
  const info = await shard.secrets.set("token", { value: "s3cr3t", destinations: ["api.example.com"] });
  assert.equal(info.placeholder, "ph_1");
  assert.deepEqual(JSON.parse(sent("PUT", "/v0/secrets/token").body), { value: "s3cr3t", destinations: ["api.example.com"] });
  await shard.secrets.remove("token", { force: true });
  assert.equal(sent("DELETE", "/v0/secrets/token").url.searchParams.get("force"), "true");
  assert.deepEqual((await shard.secrets.grant("sb_1", "token")).secrets, ["token"]);
  assert.deepEqual((await shard.secrets.ungrant("sb_1", "token")).secrets, []);
});

test("a snapshot create names its sandbox by id", async () => {
  const sandbox = await shard.create({ image: "alpine" });
  routes.set("POST /v0/snapshots", () => ({
    status: 201,
    json: {
      id: "sn_1",
      name: "snap",
      source: "sb_1",
      image: "alpine",
      digest: "sha256:abc",
      provider: "gvisor",
      disk_mib: 1024,
      memory_mib: 512,
      size: 4096,
      created_at: "2026-10-04T10:00:00Z",
    },
  }));
  const snapshot = await shard.snapshots.create(sandbox, { name: "snap" });
  assert.equal(snapshot.id, "sn_1");
  assert.equal(snapshot.sourceName, null);
  assert.deepEqual(JSON.parse(sent("POST", "/v0/snapshots").body), { sandbox: "sb_1", name: "snap" });
});
