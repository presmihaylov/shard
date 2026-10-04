import assert from "node:assert/strict";
import { test } from "node:test";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";
import { OutputCapture, defaultOutputLimit } from "../src/capture.js";
import { stderr, stdout } from "../src/wire.js";

const text = (capture: OutputCapture) => {
  const { stdout: out, stderr: err } = capture.output();

  return { out: out.toString(), err: err.toString() };
};

test("the default limit is 8 MiB", () => {
  assert.equal(defaultOutputLimit, 8 * 1024 * 1024);
});

test("the limit spans stdout and stderr together, newest bytes kept", () => {
  const capture = new OutputCapture(10);
  capture.add(stdout, Buffer.from("aaaa"));
  capture.add(stderr, Buffer.from("bbbb"));
  capture.add(stdout, Buffer.from("cccc"));
  assert.deepEqual(text(capture), { out: "aacccc", err: "bbbb" });
  capture.add(stderr, Buffer.from("dddddd"));
  assert.deepEqual(text(capture), { out: "cccc", err: "dddddd" });
});

test("a chunk at or over the limit keeps only its tail", () => {
  const capture = new OutputCapture(4);
  capture.add(stderr, Buffer.from("old"));
  capture.add(stdout, Buffer.from("0123456789"));
  assert.deepEqual(text(capture), { out: "6789", err: "" });
  capture.add(stderr, Buffer.from("WXYZ"));
  assert.deepEqual(text(capture), { out: "", err: "WXYZ" });
});

test("a zero limit keeps nothing", () => {
  const capture = new OutputCapture(0);
  capture.add(stdout, Buffer.from("anything"));
  assert.deepEqual(text(capture), { out: "", err: "" });
});

test("reset drops everything", () => {
  const capture = new OutputCapture(100);
  capture.add(stdout, Buffer.from("before"));
  capture.reset();
  capture.add(stdout, Buffer.from("after"));
  assert.deepEqual(text(capture), { out: "after", err: "" });
});

test("many small chunks hold exactly the newest bytes", () => {
  const capture = new OutputCapture(5000);
  let all = "";
  for (let i = 0; i < 20000; i++) {
    const chunk = `${i % 10}`;
    all += chunk;
    capture.add(i % 3 === 0 ? stderr : stdout, Buffer.from(chunk));
  }
  const { stdout: out, stderr: err } = capture.output();
  assert.equal(out.length + err.length, 5000);
  const kept = all.slice(-5000);
  const start = 20000 - 5000;
  const expected = { out: "", err: "" };
  for (let i = 0; i < kept.length; i++) {
    const at = start + i;
    if (at % 3 === 0) {
      expected.err += kept[i];
      continue;
    }
    expected.out += kept[i];
  }
  assert.deepEqual(text(capture), expected);
});

test("a dropped chunk is released at once, long before the array is trimmed", async () => {
  setFlagsFromString("--expose-gc");
  const gc: unknown = runInNewContext("gc");
  assert.ok(typeof gc === "function", "this node exposes no gc");
  const capture = new OutputCapture(8);
  const dropped = new WeakRef(new Uint8Array(4));
  capture.add(stdout, dropped.deref() ?? new Uint8Array(0));
  capture.add(stdout, new Uint8Array(4));
  capture.add(stdout, new Uint8Array(4));
  // A WeakRef holds its target until the current job ends.
  await new Promise((resolve) => setImmediate(resolve));
  gc();
  assert.equal(dropped.deref(), undefined);
  assert.equal(capture.output().stdout.length, 8);
});

test("a limit that is not a whole number of 0 or more is refused", () => {
  for (const limit of [-1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
    assert.throws(() => new OutputCapture(limit), RangeError);
  }
});
