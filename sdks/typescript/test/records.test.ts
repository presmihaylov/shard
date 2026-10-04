import assert from "node:assert/strict";
import { test } from "node:test";
import { ProtocolError } from "../src/errors.js";
import { appExit, networkLogRecord, policy, records, sandboxInfo } from "../src/records.js";
import { sandboxRecord } from "./helpers/records.js";

test("a sandbox create made runs no app", () => {
  const info = sandboxInfo(sandboxRecord());
  assert.equal(info.app, null);
  assert.equal(info.policy, null);
  assert.equal(info.snapshot, null);
  assert.deepEqual(info.resources, { memoryMiB: 512, vcpus: 1, diskMiB: 1024 });
  assert.equal(info.createdAt.toISOString(), "2026-10-04T10:00:00.123Z");
});

test("a run's app carries its exit and its restart policy, and a signal of 0 is none", () => {
  const info = sandboxInfo(
    sandboxRecord({
      command: ["/bin/sh", "-c", "exit 3"],
      exit_status: { code: 3, signal: 0 },
      restart: { policy: "on-failure", retries: 2, backoff: 1, count: 2, last_at: "2026-10-04T10:00:05Z", gave_up: true, ended: true },
    }),
  );
  assert.deepEqual(info.app?.command, ["/bin/sh", "-c", "exit 3"]);
  assert.deepEqual(info.app?.exitStatus, { code: 3, signal: null });
  assert.equal(info.app?.restart?.gaveUp, true);
  assert.equal(info.app?.restart?.lastAt?.toISOString(), "2026-10-04T10:00:05.000Z");
  assert.equal(sandboxInfo(sandboxRecord({ command: ["sleep", "9"] })).app?.restart, null);
});

test("an always policy has no retries on the wire", () => {
  const restart = { policy: "always", backoff: 1, count: 0, gave_up: false, ended: false };
  assert.equal(sandboxInfo(sandboxRecord({ command: ["sleep", "9"], restart })).app?.restart?.retries, 0);
});

test("a sandbox state the SDK does not know is refused", () => {
  assert.throws(() => sandboxInfo(sandboxRecord({ state: "exploded" })), ProtocolError);
});

test("an app exit by signal names it", () => {
  assert.deepEqual(appExit({ code: 137, signal: 9, restarts: 1 }), { exitCode: 137, signal: 9, restarts: 1 });
  assert.deepEqual(appExit({ code: 0, signal: 0, restarts: 0 }), { exitCode: 0, signal: null, restarts: 0 });
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

test("a network log record with no port names none", () => {
  const read = networkLogRecord({ time: "2026-10-04T10:00:00Z", source: "dns", verdict: "deny", host: "example.com", rule: "default" });
  assert.equal(read.port, null);
  assert.equal(read.address, null);
  assert.equal(read.time.toISOString(), "2026-10-04T10:00:00.000Z");
});

test("an empty list may come as null, and anything else but an array is refused", () => {
  assert.deepEqual(records(null, "the log", networkLogRecord), []);
  assert.throws(() => records({}, "the log", networkLogRecord), ProtocolError);
});
