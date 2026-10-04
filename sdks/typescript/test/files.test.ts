import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { Readable } from "node:stream";
import { afterEach, beforeEach, test } from "node:test";
import { pack, unpack } from "../src/archive.js";
import { NotFoundError, ProtocolError, UnknownLengthError } from "../src/errors.js";
import { Files } from "../src/files.js";
import { Transport } from "../src/transport.js";
import { FakeDaemon, type Answer, type Request } from "./helpers/daemon.js";

const files = "/v0/sandboxes/sb_1/files";
const archive = "/v0/sandboxes/sb_1/archive";
const mtime = "2026-10-04T10:00:00Z";

let daemon: FakeDaemon;
let transport: Transport;
let sandboxFiles: Files;
let routes: Map<string, (request: Request) => Answer>;
let dir: string;

beforeEach(async () => {
  daemon = await FakeDaemon.start();
  transport = new Transport({ baseUrl: daemon.url, apiKey: "test-key", ca: undefined });
  sandboxFiles = new Files(transport, "sb_1");
  routes = new Map();
  daemon.route = (request) => routes.get(`${request.method} ${request.url.pathname}`)?.(request);
  dir = await fs.mkdtemp(path.join(tmpdir(), "useshards-files-"));
});

afterEach(async () => {
  transport.close();
  await daemon.close();
  await fs.rm(dir, { recursive: true, force: true });
});

function stat(fields: Record<string, unknown> = {}): Record<string, string> {
  return { "X-Shard-Stat": JSON.stringify({ type: "file", size: 5, mode: 0o640, uid: 0, gid: 0, mtime, ...fields }) };
}

function sent(method: string, pathname: string): Request {
  const request = daemon.requests.find((r) => r.method === method && r.url.pathname === pathname);
  assert.ok(request, `the daemon got ${method} ${pathname}`);

  return request;
}

async function collect(source: AsyncIterable<Uint8Array>): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of source) {
    chunks.push(Buffer.from(chunk));
  }

  return Buffer.concat(chunks);
}

test("a read answers the file's bytes, or its text", async () => {
  routes.set(`GET ${files}`, () => ({ status: 200, raw: "hello" }));
  assert.deepEqual(Buffer.from(await sandboxFiles.read("/etc/motd")), Buffer.from("hello"));
  assert.equal(await sandboxFiles.read("/etc/motd", { encoding: "utf8" }), "hello");
  assert.equal(sent("GET", files).url.searchParams.get("path"), "/etc/motd");
});

test("a write sends the bytes with their length, and the mode in octal", async () => {
  routes.set(`PUT ${files}`, () => ({ status: 204 }));
  await sandboxFiles.write("/srv/a.txt", "hello", { mode: 0o644, parents: true, user: "nobody" });
  const request = sent("PUT", files);
  assert.equal(request.body, "hello");
  assert.equal(request.headers["content-length"], "5");
  assert.deepEqual(Object.fromEntries(request.url.searchParams), { path: "/srv/a.txt", mode: "644", parents: "true", user: "nobody" });
});

test("a stat reads the X-Shard-Stat header of a HEAD", async () => {
  routes.set(`HEAD ${files}`, () => ({ status: 200, headers: stat({ type: "dir", mode: 0o1777 }) }));
  const info = await sandboxFiles.stat("/tmp");
  assert.equal(info.type, "dir");
  assert.equal(info.mode, 0o1777);
  assert.equal(info.mtime.toISOString(), "2026-10-04T10:00:00.000Z");
  routes.set(`HEAD ${files}`, () => ({ status: 200 }));
  await assert.rejects(sandboxFiles.stat("/tmp"), ProtocolError);
  routes.set(`HEAD ${files}`, () => ({ status: 404 }));
  await assert.rejects(sandboxFiles.stat("/nope"), (err) => err instanceof NotFoundError && err.message.includes("a stat of /nope"));
});

test("a list answers each entry with its own stat, and refuses a cut listing", async () => {
  const entry = { name: "a.txt", type: "file", size: 5, mode: 0o644, uid: 1000, gid: 1000, mtime };
  routes.set("GET /v0/sandboxes/sb_1/ls", () => ({ status: 200, json: { entries: [entry] } }));
  const [listed] = await sandboxFiles.list("/srv");
  assert.equal(listed?.name, "a.txt");
  assert.equal(listed?.uid, 1000);
  routes.set("GET /v0/sandboxes/sb_1/ls", () => ({ status: 200, raw: '{"entries":[{"name":' }));
  await assert.rejects(sandboxFiles.list("/srv"), /cut the listing of \/srv short/);
});

test("mkdir and a recursive remove", async () => {
  routes.set("POST /v0/sandboxes/sb_1/mkdir", () => ({ status: 204 }));
  routes.set(`DELETE ${files}`, () => ({ status: 204 }));
  await sandboxFiles.mkdir("/srv/a/b", { mode: 0o755, parents: true });
  assert.deepEqual(JSON.parse(sent("POST", "/v0/sandboxes/sb_1/mkdir").body), { path: "/srv/a/b", mode: "755", parents: true });
  await sandboxFiles.remove("/srv/a", { recursive: true });
  assert.deepEqual(Object.fromEntries(sent("DELETE", files).url.searchParams), { path: "/srv/a", recursive: "true" });
});

test("an upload streams a local file with its length and its mode", async () => {
  routes.set(`PUT ${files}`, () => ({ status: 204 }));
  const local = path.join(dir, "big.bin");
  const data = Buffer.alloc(3 << 20, 7);
  await fs.writeFile(local, data, { mode: 0o640 });
  await sandboxFiles.upload(local, "/srv/big.bin");
  const request = sent("PUT", files);
  assert.equal(request.headers["content-length"], String(data.length));
  assert.ok(request.bytes.equals(data));
  assert.equal(request.url.searchParams.get("mode"), "640");
  await assert.rejects(sandboxFiles.upload(dir, "/srv/d"), UnknownLengthError);
});

test("a download lands the file whole, at the sandbox's mode, through a temp name", async () => {
  routes.set(`GET ${files}`, () => ({ status: 200, raw: "hello", headers: stat({ mode: 0o600 }) }));
  const local = path.join(dir, "out.txt");
  await sandboxFiles.download("/srv/a.txt", local);
  assert.equal(await fs.readFile(local, "utf8"), "hello");
  assert.equal((await fs.stat(local)).mode & 0o777, 0o600);
  assert.deepEqual(await fs.readdir(dir), ["out.txt"]);
});

test("a download with no stat lands nothing", async () => {
  routes.set(`GET ${files}`, () => ({ status: 200, raw: "hello" }));
  await assert.rejects(sandboxFiles.download("/srv/a.txt", path.join(dir, "out.txt")), ProtocolError);
  assert.deepEqual(await fs.readdir(dir), []);
});

test("an uploadDir sends a chunked tar of the directory under its sandbox name", async () => {
  routes.set(`PUT ${archive}`, () => ({ status: 204 }));
  const local = path.join(dir, "src");
  await fs.mkdir(local);
  await fs.writeFile(path.join(local, "a.txt"), "hello");
  await sandboxFiles.uploadDir(local, "/srv/app/");
  const request = sent("PUT", archive);
  assert.equal(request.url.searchParams.get("path"), "/srv");
  assert.equal(request.headers["transfer-encoding"], "chunked");
  const landed = path.join(dir, "landed");
  await fs.mkdir(landed);
  await unpack(Readable.from([request.bytes]), landed, "app");
  assert.equal(await fs.readFile(path.join(landed, "a.txt"), "utf8"), "hello");
  await assert.rejects(sandboxFiles.uploadDir(local, "srv"), TypeError);
});

test("a downloadDir lands the sandbox's tree as local, and refuses a path that is not a directory", async () => {
  const tree = path.join(dir, "tree");
  await fs.mkdir(tree);
  await fs.writeFile(path.join(tree, "a.txt"), "hello");
  const tar = await collect(pack(tree, "app"));
  routes.set(`GET ${archive}`, () => ({ status: 200, raw: tar, headers: stat({ type: "dir", mode: 0o750 }) }));
  const local = path.join(dir, "out");
  await sandboxFiles.downloadDir("/srv/app", local);
  assert.equal(await fs.readFile(path.join(local, "a.txt"), "utf8"), "hello");
  assert.equal((await fs.stat(local)).mode & 0o777, 0o750);
  assert.equal(sent("GET", archive).url.searchParams.get("path"), "/srv/app");
  routes.set(`GET ${archive}`, () => ({ status: 200, raw: tar, headers: stat() }));
  await assert.rejects(sandboxFiles.downloadDir("/srv/a.txt", path.join(dir, "other")), { code: "ENOTDIR" });
  await assert.rejects(fs.stat(path.join(dir, "other")), { code: "ENOENT" });
});
