import assert from "node:assert/strict";
import { test } from "node:test";
import { ProtocolError } from "../src/errors.js";
import { egressDecision, policy, port, processInfo, records, sandboxInfo } from "../src/records.js";
import { portRecord, processRecord, sandboxRecord } from "./helpers/records.js";

test("a sandbox create made runs no process", () => {
  const info = sandboxInfo(sandboxRecord());
  assert.deepEqual(info.processes, []);
  assert.equal(info.policy, null);
  assert.equal(info.snapshot, null);
  assert.deepEqual(info.resources, { memoryMiB: 512, vcpus: 1, diskMiB: 1024, swapMiB: 0 });
  assert.equal(info.createdAt.toISOString(), "2026-10-04T10:00:00.123Z");
});

test("a fork names the sandbox it was forked from", () => {
  assert.equal(sandboxInfo(sandboxRecord({ forked_from: "sb_1" })).forkedFrom, "sb_1");
  assert.equal(sandboxInfo(sandboxRecord()).forkedFrom, null);
});

test("a process carries its policy and its last exit, and a signal of 0 is none", () => {
  const info = sandboxInfo(
    sandboxRecord({
      processes: [
        processRecord({
          restart: { policy: "on-failure", retries: 2, backoff: 1 },
          status: { state: "gave-up", restarts: 2, exit: { code: 3, signal: 0 }, started_at: "2026-10-04T10:00:05Z" },
        }),
      ],
    }),
  );
  const [web] = info.processes;
  assert.deepEqual(web?.command, ["/bin/sh", "-c", "serve"]);
  assert.deepEqual(web?.restart, { policy: "on-failure", retries: 2, backoff: 1 });
  assert.deepEqual(web?.status.exit, { exitCode: 3, signal: null });
  assert.equal(web?.status.state, "gave-up");
  assert.equal(web?.status.startedAt?.toISOString(), "2026-10-04T10:00:05.000Z");
});

test("a running process has no exit, and the wire leaves out what it does not set", () => {
  const info = processInfo(processRecord());
  assert.deepEqual(info.restart, { policy: "unless-stopped", retries: 0, backoff: 0 });
  assert.equal(info.status.exit, null);
  assert.equal(info.killed, false);
  assert.deepEqual(info.env, []);
  assert.equal(info.workdir, null);
  assert.equal(info.user, null);
});

test("a process state the SDK does not know is refused", () => {
  assert.throws(() => processInfo(processRecord({ status: { state: "exploded", restarts: 0 } })), ProtocolError);
});

test("a sandbox state the SDK does not know is refused", () => {
  assert.throws(() => sandboxInfo(sandboxRecord({ state: "exploded" })), ProtocolError);
});

test("a policy's rules come back spelled as a set takes them", () => {
  const read = policy({
    name: "web",
    rules: [
      { action: "allow", destination: { kind: "domain-suffix", value: "example.com" }, protocol: "tcp", ports: [443, 8443] },
      { action: "allow", destination: { kind: "domain", value: "api.example.com" } },
      { action: "deny", destination: { kind: "cidr", value: "10.0.0.0/8" }, protocol: "udp" },
      { action: "allow", destination: { kind: "group", value: "github" } },
    ],
    holders: ["sb_1"],
    dns: "open",
  });
  assert.deepEqual(read.rules, [
    { action: "allow", rule: "suffix:example.com tcp:443,8443" },
    { action: "allow", rule: "api.example.com" },
    { action: "deny", rule: "10.0.0.0/8 udp" },
    { action: "allow", rule: "github" },
  ]);
  assert.deepEqual(read.holders, ["sb_1"]);
  assert.equal(read.dns, "open");
});

test("a listed policy has no holders and no dns", () => {
  const read = policy({ name: "web", rules: [] });
  assert.equal(read.holders, null);
  assert.equal(read.dns, null);
});

test("an egress decision with no port names none", () => {
  const read = egressDecision({ time: "2026-10-04T10:00:00Z", source: "dns", verdict: "deny", host: "example.com", rule: "default" });
  assert.equal(read.port, null);
  assert.equal(read.address, null);
  assert.equal(read.time.toISOString(), "2026-10-04T10:00:00.000Z");
});

test("a sandbox record names its forwards, and none when it has none", () => {
  const ports = [{ host_port: 9000, guest_port: 8000, public: true }, { host_port: 9001, guest_port: 8001 }];
  assert.deepEqual(sandboxInfo(sandboxRecord({ ports })).ports, [
    { hostPort: 9000, guestPort: 8000, public: true },
    { hostPort: 9001, guestPort: 8001, public: false },
  ]);
  assert.deepEqual(sandboxInfo(sandboxRecord()).ports, []);
});

test("a port that does not listen says why, and one with no reachable address answers none", () => {
  const read = port(portRecord({ listening: false, error: "sandbox web is not running", reachable_on: undefined }));
  assert.equal(read.listening, false);
  assert.equal(read.error, "sandbox web is not running");
  assert.deepEqual(read.reachableOn, []);
  assert.throws(() => port(portRecord({ host_port: "9000" })), ProtocolError);
});

test("an empty list may come as null, and anything else but an array is refused", () => {
  assert.deepEqual(records(null, "the log", egressDecision), []);
  assert.throws(() => records({}, "the log", egressDecision), ProtocolError);
});
