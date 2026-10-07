// Run with SHARD_REMOTE, and SHARD_API_KEY and SHARD_SUITE_WILDCARD_KEY both "*" tokens, since the secret, policy and log checks need every scope; SHARD_SUITE_ONLY=name,name runs a subset and SHARD_SUITE_IMAGE picks the image.
import { readFile } from "node:fs/promises";
import { checks as auth } from "./checks/auth.js";
import { checks as capture } from "./checks/capture.js";
import { checks as commands } from "./checks/commands.js";
import { checks as files } from "./checks/files.js";
import { checks as lifecycle } from "./checks/lifecycle.js";
import { checks as logs } from "./checks/logs.js";
import { checks as policies } from "./checks/policies.js";
import { checks as ports } from "./checks/ports.js";
import { checks as processes } from "./checks/processes.js";
import { checks as secrets } from "./checks/secrets.js";
import { checks as snapshots } from "./checks/snapshots.js";
import { Context, Skip, describe, type Check } from "./harness.js";

const registry: Check[] = [
  ...auth,
  ...lifecycle,
  ...snapshots,
  ...commands,
  ...capture,
  ...files,
  ...processes,
  ...logs,
  ...secrets,
  ...policies,
  ...ports,
];

const checkTimeoutMs = 300_000;

// A rejection nothing awaited belongs to the check that was running, or to the run once the last check ended.
const strays: unknown[] = [];

/** Refusal is a run the suite will not start, which it reports without a stack. */
class Refusal extends Error {}

async function main(): Promise<number> {
  for (const key of ["SHARD_REMOTE", "SHARD_API_KEY"]) {
    if (!process.env[key]) {
      throw new Refusal(`${key} is not set`);
    }
  }
  await matchNames();
  const selected = select(registry, process.env.SHARD_SUITE_ONLY ?? "");
  process.on("unhandledRejection", (reason) => strays.push(reason));

  const root = Context.suite();
  const counts = { PASS: 0, FAIL: 0, SKIP: 0 };
  for (const check of selected) {
    const line = await verdict(root, check);
    counts[line.verdict]++;
    console.log(line.text);
  }
  const leftover = [
    ...(await root.cleanup()).map((what) => `cleanup: ${what}`),
    ...strays.splice(0).map((stray) => `unhandled rejection: ${describe(stray)}`),
  ];
  for (const what of leftover) {
    counts.FAIL++;
    console.log(`FAIL suite: ${what}`);
  }
  console.error(`suite: ${selected.length} checks, ${counts.PASS} passed, ${counts.FAIL} failed, ${counts.SKIP} skipped`);

  return counts.FAIL > 0 ? 1 : 0;
}

/** matchNames refuses a registry that drifted from the shared list, so both SDK suites always run the same checks. */
async function matchNames(): Promise<void> {
  const listed = (await readFile(new URL("../../suite/checks.txt", import.meta.url), "utf8")).split("\n").filter(Boolean);
  const registered = registry.map((check) => check.name);
  const missing = listed.filter((name) => !registered.includes(name));
  const extra = registered.filter((name) => !listed.includes(name));
  if (missing.length > 0 || extra.length > 0) {
    throw new Refusal(`the checks differ from sdks/suite/checks.txt: missing [${missing.join(", ")}], extra [${extra.join(", ")}]`);
  }
  const moved = listed.findIndex((name, at) => registered[at] !== name);
  if (moved >= 0) {
    throw new Refusal(`the checks run out of the order of sdks/suite/checks.txt: line ${moved + 1} is ${listed[moved]}, the suite has ${registered[moved]}`);
  }
}

function select(checks: Check[], only: string): Check[] {
  const wanted = only.split(",").map((name) => name.trim()).filter(Boolean);
  if (wanted.length === 0) {
    return checks;
  }
  const unknown = wanted.filter((name) => !checks.some((check) => check.name === name));
  if (unknown.length > 0) {
    throw new Refusal(`SHARD_SUITE_ONLY names no such check: ${unknown.join(", ")}`);
  }

  return checks.filter((check) => wanted.includes(check.name));
}

interface Line {
  verdict: "PASS" | "FAIL" | "SKIP";
  text: string;
}

/** verdict runs one check and undoes what it made; a check that passed but left something behind fails. */
async function verdict(root: Context, check: Check): Promise<Line> {
  const ctx = root.check();
  const err = await within(checkTimeoutMs, check.run(ctx)).then(
    () => undefined,
    (caught: unknown) => caught,
  );
  const failures = [
    ...(err === undefined || err instanceof Skip ? [] : [err]),
    ...(await ctx.cleanup()).map((what) => `cleanup: ${what}`),
    ...strays.splice(0).map((stray) => `unhandled rejection: ${describe(stray)}`),
  ];
  if (failures.length > 0) {
    for (const failure of failures) {
      console.error(`--- ${check.name}\n${failure instanceof Error ? (failure.stack ?? describe(failure)) : String(failure)}`);
    }
    const first = failures[0] instanceof Error ? describe(failures[0]) : String(failures[0]);

    return { verdict: "FAIL", text: `FAIL ${check.name}: ${first.split("\n")[0]}` };
  }
  if (err instanceof Skip) {
    return { verdict: "SKIP", text: `SKIP ${check.name}: ${err.message}` };
  }

  return { verdict: "PASS", text: `PASS ${check.name}` };
}

function within(ms: number, work: Promise<void>): Promise<void> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`the check did not end within ${ms / 1000} s`)), ms);
  });

  return Promise.race([work, timeout]).finally(() => clearTimeout(timer));
}

/** exit waits for stdout to drain, which a pipe on macOS does not do on its own, then closes any open socket. */
function exit(code: number): void {
  process.stdout.write("", () => process.exit(code));
}

main().then(exit, (err: unknown) => {
  console.error(`suite: ${err instanceof Refusal ? err.message : describe(err)}`);
  exit(1);
});
