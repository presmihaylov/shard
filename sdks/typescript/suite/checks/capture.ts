import assert from "node:assert/strict";
import type { ExecOptions, Sandbox } from "useshards";
import { MiB, type Check } from "../harness.js";

type Stream = "stdout" | "stderr";

interface Block {
  stream: Stream;
  tag: string;
  size: number;
}

/** Arrival is one chunk a callback got, in the order the SDK got it. */
interface Arrival {
  stream: Stream;
  bytes: Buffer;
}

/** blockText is what `yes <tag> | head -c <size>` prints, so the suite knows every byte the writer sends. */
function blockText(block: Block): string {
  const line = `${block.tag}\n`;

  return line.repeat(Math.ceil(block.size / line.length)).slice(0, block.size);
}

function writer(blocks: Block[]): string {
  return blocks
    .map((block) => `yes ${block.tag} | head -c ${block.size}${block.stream === "stderr" ? " >&2" : ""}`)
    .join("; ");
}

/** alternate is n blocks of size bytes each, stdout first, so the two streams interleave. */
function alternate(n: number, size: number): Block[] {
  return Array.from({ length: n }, (_, i) => {
    const stream: Stream = i % 2 === 0 ? "stdout" : "stderr";

    return { stream, tag: `${stream === "stdout" ? "o" : "e"}${i}`, size };
  });
}

function expected(blocks: Block[], stream: Stream): string {
  return blocks
    .filter((block) => block.stream === stream)
    .map(blockText)
    .join("");
}

/** newest is the reference capture: the last limit bytes of the arrivals, both streams counted together. */
function newest(arrivals: Arrival[], limit: number): Record<Stream, string> {
  const kept: Arrival[] = [];
  let left = limit;
  for (const { stream, bytes } of [...arrivals].reverse()) {
    if (left === 0) {
      break;
    }
    const take = Math.min(left, bytes.length);
    kept.unshift({ stream, bytes: bytes.subarray(bytes.length - take) });
    left -= take;
  }
  const join = (stream: Stream) =>
    Buffer.concat(kept.filter((each) => each.stream === stream).map((each) => each.bytes)).toString("utf8");

  return { stdout: join("stdout"), stderr: join("stderr") };
}

/** run executes the writer with callbacks that record every chunk, and checks they got every byte it wrote. */
async function run(sandbox: Sandbox, blocks: Block[], options: ExecOptions) {
  const arrivals: Arrival[] = [];
  const result = await sandbox.exec(writer(blocks), {
    ...options,
    onStdout: (chunk) => arrivals.push({ stream: "stdout", bytes: Buffer.from(chunk) }),
    onStderr: (chunk) => arrivals.push({ stream: "stderr", bytes: Buffer.from(chunk) }),
  });
  assert.equal(result.exitCode, 0, result.stderr.slice(-200));
  for (const stream of ["stdout", "stderr"] as const) {
    const got = Buffer.concat(arrivals.filter((each) => each.stream === stream).map((each) => each.bytes)).toString("utf8");
    assert.ok(got === expected(blocks, stream), `the ${stream} callback got ${got.length} bytes, not every byte written`);
  }

  return { result, arrivals };
}

export const checks: Check[] = [
  {
    name: "capture.default_limit",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const blocks: Block[] = Array.from({ length: 9 }, (_, i) => ({ stream: "stdout", tag: `d${i}`, size: MiB }));
      const result = await sandbox.exec(writer(blocks));
      assert.equal(result.stdout.length, 8 * MiB, "the default keeps 8 MiB");
      assert.ok(result.stdout === blocks.slice(1).map(blockText).join(""), "the default keeps the newest 8 MiB");
      assert.equal(result.stderr, "");
    },
  },
  {
    name: "capture.newest_bytes",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const limit = 8 * MiB;
      const blocks = alternate(40, MiB / 2);
      const { result, arrivals } = await run(sandbox, blocks, { outputLimitBytes: limit });
      const want = newest(arrivals, limit);
      assert.equal(result.stdout.length + result.stderr.length, limit, "stdout and stderr share one limit");
      assert.ok(result.stdout === want.stdout, "stdout holds its part of the newest 8 MiB");
      assert.ok(result.stderr === want.stderr, "stderr holds its part of the newest 8 MiB");
    },
  },
  {
    name: "capture.zero_disables",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const { result } = await run(sandbox, alternate(4, MiB / 2), { outputLimitBytes: 0 });
      assert.equal(result.stdout, "");
      assert.equal(result.stderr, "");
    },
  },
  {
    name: "capture.callbacks_every_chunk",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const limit = MiB;
      const blocks = alternate(8, MiB / 2);
      const { result, arrivals } = await run(sandbox, blocks, { outputLimitBytes: limit });
      const total = arrivals.reduce((sum, each) => sum + each.bytes.length, 0);
      assert.equal(total - limit, 3 * MiB, "the callbacks got the 3 MiB past the limit too");
      const want = newest(arrivals, limit);
      assert.ok(result.stdout === want.stdout && result.stderr === want.stderr, "the capture still keeps the newest 1 MiB");
    },
  },
];
