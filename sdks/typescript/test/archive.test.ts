import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import * as fs from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import { promisify } from "node:util";
import { maxBytes, pack, unpack } from "../src/archive.js";
import { UnsafeArchiveError } from "../src/errors.js";
import * as tar from "../src/tar.js";

const run = promisify(execFile);

interface Entry extends Partial<tar.Header> {
  name: string;
  data?: string;
}

/** archive builds a tar by hand, so a test can write what no honest packer would. */
function archive(...entries: Entry[]): Buffer {
  const blocks = entries.map(({ data = "", ...fields }) => {
    const body = Buffer.from(data);
    const header: tar.Header = { mode: 0o644, uid: 0, gid: 0, mtime: 0, type: tar.typeFile, linkname: "", ...fields, size: body.length };

    return Buffer.concat([tar.encodeHeader(header), body, tar.padding(body.length)]);
  });

  return Buffer.concat([...blocks, tar.end]);
}

async function* chunks(buffer: Buffer, size = 700): AsyncGenerator<Uint8Array> {
  for (let at = 0; at < buffer.length; at += size) {
    yield buffer.subarray(at, at + size);
  }
}

async function collect(source: AsyncIterable<Buffer>): Promise<Buffer> {
  const parts: Buffer[] = [];
  for await (const part of source) {
    parts.push(part);
  }

  return Buffer.concat(parts);
}

async function scratch(t: { after: (fn: () => Promise<void>) => void }): Promise<string> {
  const at = await fs.mkdtemp(path.join(tmpdir(), "useshards-archive-"));
  t.after(() => fs.rm(at, { recursive: true, force: true }));

  return at;
}

/** tree lists every path under root with its type, mode and content, the shape a round trip must keep. */
async function tree(root: string): Promise<string[]> {
  const lines: string[] = [];
  const walk = async (at: string, rel: string): Promise<void> => {
    for (const name of (await fs.readdir(at)).sort()) {
      const full = path.join(at, name);
      const info = await fs.lstat(full);
      const mode = (info.mode & 0o7777).toString(8);
      const sub = rel ? `${rel}/${name}` : name;
      if (info.isSymbolicLink()) {
        lines.push(`${sub} link ${await fs.readlink(full)}`);
        continue;
      }
      if (info.isDirectory()) {
        lines.push(`${sub} dir ${mode}`);
        await walk(full, sub);
        continue;
      }
      lines.push(`${sub} file ${mode} ${await fs.readFile(full, "utf8")}`);
    }
  };
  await walk(root, "");

  return lines;
}

async function unpacked(t: { after: (fn: () => Promise<void>) => void }, buffer: Buffer, strip = "top"): Promise<string> {
  const dst = path.join(await scratch(t), "dst");
  await fs.mkdir(dst);
  await unpack(chunks(buffer), dst, strip);

  return dst;
}

async function refused(t: { after: (fn: () => Promise<void>) => void }, buffer: Buffer, entry: string, reason: string | RegExp): Promise<string> {
  const dst = path.join(await scratch(t), "dst");
  await fs.mkdir(dst);
  await assert.rejects(unpack(chunks(buffer), dst, "top"), (err) => {
    assert.ok(err instanceof UnsafeArchiveError, `${String(err)} is not an UnsafeArchiveError`);
    assert.equal(err.entry, entry);
    if (typeof reason === "string") {
      assert.equal(err.reason, reason);
    }
    if (reason instanceof RegExp) {
      assert.match(err.reason, reason);
    }

    return true;
  });

  return dst;
}

async function source(t: { after: (fn: () => Promise<void>) => void }): Promise<string> {
  const src = path.join(await scratch(t), "src");
  await fs.mkdir(path.join(src, "nested", "deeper"), { recursive: true });
  await fs.mkdir(path.join(src, "empty"));
  await fs.writeFile(path.join(src, "a.txt"), "alpha");
  await fs.writeFile(path.join(src, "nested", "b.txt"), "beta");
  await fs.writeFile(path.join(src, "nested", "deeper", `${"long".repeat(40)}.txt`), "a long name");
  await fs.writeFile(path.join(src, "run.sh"), "#!/bin/sh");
  await fs.symlink("nested/b.txt", path.join(src, "inner"));
  await fs.chmod(path.join(src, "run.sh"), 0o750);
  await fs.chmod(path.join(src, "nested"), 0o751);

  return src;
}

test("a packed tree unpacks to the same tree", async (t) => {
  const src = await source(t);
  const dst = await unpacked(t, await collect(pack(src, "top")));
  assert.deepEqual(await tree(dst), await tree(src));
});

test("pack orders entries by their bytes and marks a directory with a slash", async (t) => {
  const src = path.join(await scratch(t), "src");
  await fs.mkdir(path.join(src, "b"), { recursive: true });
  // C and not B, as a case-folding filesystem holds b and B as one name.
  for (const name of ["a", "C", "é", "_"]) {
    await fs.writeFile(path.join(src, name), name);
  }
  const reader = new tar.TarReader(chunks(await collect(pack(src, "top"))));
  const names: string[] = [];
  for (let next = await reader.next(); next; next = await reader.next()) {
    names.push(next.name);
  }
  assert.deepEqual(names, ["top/", "top/C", "top/_", "top/a", "top/b/", "top/é"]);
});

test("pack starts past a link the source names and keeps every link inside as a link", async (t) => {
  const src = await source(t);
  const alias = path.join(path.dirname(src), "alias");
  await fs.symlink(src, alias);
  const dst = await unpacked(t, await collect(pack(alias, "top")));
  assert.deepEqual(await tree(dst), await tree(src));
  assert.equal(await fs.readlink(path.join(dst, "inner")), "nested/b.txt");
});

test("pack leaves a socket out", async (t) => {
  const src = path.join(await scratch(t), "s");
  await fs.mkdir(src);
  await fs.writeFile(path.join(src, "kept"), "k");
  const server = createServer();
  await new Promise<void>((resolve) => server.listen(path.join(src, "sock"), resolve));
  t.after(() => new Promise<void>((resolve) => server.close(() => resolve())));
  const dst = await unpacked(t, await collect(pack(src, "top")));
  assert.deepEqual(await tree(dst), ["kept file 644 k"]);
});

test("the system tar reads what pack writes", async (t) => {
  const src = await source(t);
  const out = await scratch(t);
  await fs.writeFile(path.join(out, "a.tar"), await collect(pack(src, "top")));
  await run("tar", ["-xf", "a.tar"], { cwd: out });
  assert.deepEqual(await tree(path.join(out, "top")), await tree(src));
});

test("unpack reads what the system tar writes", async (t) => {
  const src = await source(t);
  const parent = path.dirname(src);
  await fs.rename(src, path.join(parent, "top"));
  // COPYFILE_DISABLE keeps macOS tar from adding ._ entries for extended attributes.
  const { stdout } = await run("tar", ["-cf", "-", "top"], { cwd: parent, encoding: "buffer", env: { ...process.env, COPYFILE_DISABLE: "1" } });
  const dst = await unpacked(t, stdout);
  assert.deepEqual(await tree(dst), await tree(path.join(parent, "top")));
});

test("unpack drops setuid and setgid and keeps the sticky bit", async (t) => {
  const dst = await unpacked(
    t,
    archive({ name: "top/", type: tar.typeDir, mode: 0o755 }, { name: "top/suid", mode: 0o6755, data: "x" }, { name: "top/tmp/", type: tar.typeDir, mode: 0o1777 }),
  );
  assert.deepEqual(await tree(dst), ["suid file 755 x", "tmp dir 1777"]);
});

test("unpack makes missing parents and sets a directory's mode once its entries are in", async (t) => {
  const dst = await unpacked(t, archive({ name: "top/ro/", type: tar.typeDir, mode: 0o555 }, { name: "top/ro/f", data: "in" }, { name: "top/a/b/c", data: "deep" }));
  try {
    assert.deepEqual(await tree(dst), ["a dir 755", "a/b dir 755", "a/b/c file 644 deep", "ro dir 555", "ro/f file 644 in"]);
  } finally {
    // The scratch removal cannot empty a directory no one may write.
    await fs.chmod(path.join(dst, "ro"), 0o755);
  }
});

test("a hard link to a file earlier in the archive shares that file", async (t) => {
  const dst = await unpacked(t, archive({ name: "top/f", data: "same" }, { name: "top/h", type: tar.typeHardlink, linkname: "top/f" }));
  const [file, link] = await Promise.all([fs.stat(path.join(dst, "f")), fs.stat(path.join(dst, "h"))]);
  assert.equal(link.ino, file.ino);
});

test("a later entry replaces a link at its name and never writes through it", async (t) => {
  const outside = path.join(await scratch(t), "outside");
  await fs.writeFile(outside, "untouched");
  const dst = await unpacked(t, archive({ name: "top/l", type: tar.typeSymlink, linkname: "f" }, { name: "top/l", data: "new" }));
  assert.deepEqual(await tree(dst), ["l file 644 new"]);
  assert.equal(await fs.readFile(outside, "utf8"), "untouched");
});

test("a global header is skipped", async (t) => {
  const dst = await unpacked(t, archive({ name: "pax_global_header", type: tar.typeGlobal, data: "" }, { name: "top/f", data: "x" }));
  assert.deepEqual(await tree(dst), ["f file 644 x"]);
});

const refusals: Array<[string, Entry[], string, string | RegExp]> = [
  ["an empty name", [{ name: "" }], "", "an empty name"],
  ["an absolute name", [{ name: "/etc/passwd" }], "/etc/passwd", "an absolute name"],
  ["a .. component", [{ name: "top/../../escape" }], "top/../../escape", "a .. component"],
  ["a name outside the top", [{ name: "other/f" }], "other/f", "it is not under top"],
  ["a name that only shares the top's prefix", [{ name: "topper/f" }], "topper/f", "it is not under top"],
  ["a file at the destination itself", [{ name: "top", data: "x" }], "top", "only a directory can land at the destination itself"],
  ["a character device", [{ name: "top/tty", type: tar.typeChar }], "top/tty", "a device node"],
  ["a block device", [{ name: "top/sda", type: tar.typeBlock }], "top/sda", "a device node"],
  ["a fifo", [{ name: "top/pipe", type: tar.typeFifo }], "top/pipe", "a fifo, which an unpack does not make"],
  ["an unknown type", [{ name: "top/odd", type: "7" }], "top/odd", "the type '7', which an unpack does not make"],
  ["an absolute symlink", [{ name: "top/l", type: tar.typeSymlink, linkname: "/etc" }], "top/l", 'a symlink to "/etc", which leaves the destination'],
  ["a symlink that climbs out", [{ name: "top/a/l", type: tar.typeSymlink, linkname: "../../x" }], "top/a/l", 'a symlink to "../../x", which leaves the destination'],
  ["a write under a symlink", [{ name: "top/l", type: tar.typeSymlink, linkname: "sub" }, { name: "top/l/f", data: "x" }], "top/l/f", "it sits under a symlink, which an unpack never writes through"],
  ["a hard link to no earlier file", [{ name: "top/h", type: tar.typeHardlink, linkname: "top/missing" }], "top/h", 'a hard link to "top/missing", which is not a file earlier in the archive'],
  ["a hard link outside the top", [{ name: "top/h", type: tar.typeHardlink, linkname: "/etc/passwd" }], "top/h", 'a hard link to "/etc/passwd", which is not a file earlier in the archive'],
  [
    "a hard link to a symlink",
    [
      { name: "top/l", type: tar.typeSymlink, linkname: "f" },
      { name: "top/h", type: tar.typeHardlink, linkname: "top/l" },
    ],
    "top/h",
    'a hard link to "top/l", which is not a file earlier in the archive',
  ],
  [
    "a hard link to a file a symlink replaced",
    [
      { name: "top/f", data: "x" },
      { name: "top/f", type: tar.typeSymlink, linkname: "g" },
      { name: "top/h", type: tar.typeHardlink, linkname: "top/f" },
    ],
    "top/h",
    'a hard link to "top/f", which is not a file earlier in the archive',
  ],
  ["a directory over a file", [{ name: "top/x", data: "x" }, { name: "top/x/", type: tar.typeDir }], "top/x/", "a directory where something else already is"],
];

for (const [what, entries, entry, reason] of refusals) {
  test(`unpack refuses ${what}`, async (t) => {
    await refused(t, archive(...entries), entry, reason);
  });
}

test("unpack refuses a file past the byte cap before it reads the data", async (t) => {
  const header = tar.encodeHeader({ name: "top/huge", mode: 0o644, uid: 0, gid: 0, size: maxBytes + 1, mtime: 0, type: tar.typeFile, linkname: "" });
  await refused(t, Buffer.concat([header, tar.end]), "top/huge", `the archive runs past ${maxBytes} bytes`);
});

test("unpack removes a symlink that leaves through another link", async (t) => {
  const dst = await refused(
    t,
    archive({ name: "top/a", type: tar.typeSymlink, linkname: "." }, { name: "top/b", type: tar.typeSymlink, linkname: "a/.." }, { name: "top/f", data: "kept" }),
    "top/b",
    "a symlink that leaves the destination through another link",
  );
  assert.deepEqual(await tree(dst), ["a link .", "f file 644 kept"]);
});

test("a refused entry still removes a link that left through another one", async (t) => {
  const dst = await refused(
    t,
    archive({ name: "top/a", type: tar.typeSymlink, linkname: "." }, { name: "top/b", type: tar.typeSymlink, linkname: "a/.." }, { name: "/abs" }),
    "/abs",
    "an absolute name",
  );
  assert.deepEqual(await tree(dst), ["a link ."]);
});

test("a tar cut inside a file leaves no temp file behind", async (t) => {
  const whole = archive({ name: "top/f", data: "x".repeat(4000) });
  const dst = path.join(await scratch(t), "dst");
  await fs.mkdir(dst);
  await assert.rejects(unpack(chunks(whole.subarray(0, 2000)), dst, "top"), /ends inside the data/);
  assert.deepEqual(await fs.readdir(dst), []);
});

test("a hard link refuses a name a case-folding filesystem gave to a later file", async (t) => {
  const probe = await scratch(t);
  await fs.writeFile(path.join(probe, "A"), "");
  const folds = await fs.access(path.join(probe, "a")).then(
    () => true,
    () => false,
  );
  if (!folds) {
    t.skip("this filesystem tells A from a");

    return;
  }
  await refused(
    t,
    archive({ name: "top/A", data: "first" }, { name: "top/a", data: "second" }, { name: "top/h", type: tar.typeHardlink, linkname: "top/A" }),
    "top/h",
    'a hard link to "top/A", which no longer names the file the archive wrote',
  );
});
