import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { NotFoundError } from "useshards";
import { MiB, rejects, type Check, type Context } from "../harness.js";

async function localDir(ctx: Context): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), `${ctx.prefix}-`));
  ctx.defer(`remove ${dir}`, () => rm(dir, { recursive: true, force: true }));

  return dir;
}

/** tree maps every file under root to its bytes, keyed by the path relative to root. */
async function tree(root: string): Promise<Map<string, Buffer>> {
  const files = new Map<string, Buffer>();
  for (const entry of await readdir(root, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile()) {
      continue;
    }
    const path = join(entry.parentPath, entry.name);
    files.set(relative(root, path), await readFile(path));
  }

  return files;
}

export const checks: Check[] = [
  {
    name: "files.write_read",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const dir = `/tmp/${ctx.name("files")}`;
      await sandbox.files.mkdir(dir);
      await sandbox.files.write(`${dir}/hello.txt`, "hello files\n");
      assert.equal(await sandbox.files.read(`${dir}/hello.txt`, { encoding: "utf8" }), "hello files\n");
      assert.equal((await sandbox.exec(["cat", `${dir}/hello.txt`])).stdout, "hello files\n", "the guest sees the write");
      const bytes = randomBytes(3 * MiB);
      await sandbox.files.write(`${dir}/blob`, bytes);
      assert.ok(Buffer.from(await sandbox.files.read(`${dir}/blob`)).equals(bytes), "bytes round-trip unchanged");
      await rejects(NotFoundError, () => sandbox.files.read(`${dir}/missing`));
    },
  },
  {
    name: "files.metadata",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const dir = `/tmp/${ctx.name("meta")}`;
      await sandbox.files.mkdir(dir);
      await sandbox.files.write(`${dir}/script.sh`, "#!/bin/sh\necho hi\n", { mode: 0o750 });
      const file = await sandbox.files.stat(`${dir}/script.sh`);
      assert.equal(file.type, "file");
      assert.equal(file.size, 18);
      assert.equal(file.mode, 0o750);
      assert.ok(file.mtime instanceof Date && !Number.isNaN(file.mtime.getTime()));
      assert.equal((await sandbox.exec([`${dir}/script.sh`])).stdout, "hi\n", "the mode reaches the guest");
      assert.equal((await sandbox.files.stat(dir)).type, "dir");
      await rejects(NotFoundError, () => sandbox.files.stat(`${dir}/missing`));
    },
  },
  {
    name: "files.list_mkdir_remove",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const dir = `/tmp/${ctx.name("tree")}`;
      await sandbox.files.mkdir(`${dir}/a/b`, { parents: true });
      await sandbox.files.write(`${dir}/a/one.txt`, "1");
      const names = (await sandbox.files.list(`${dir}/a`)).map((entry) => `${entry.name}:${entry.type}`).sort();
      assert.deepEqual(names, ["b:dir", "one.txt:file"]);
      await sandbox.files.remove(`${dir}/a/one.txt`);
      await rejects(NotFoundError, () => sandbox.files.stat(`${dir}/a/one.txt`));
      await sandbox.files.remove(dir, { recursive: true });
      await rejects(NotFoundError, () => sandbox.files.list(dir));
    },
  },
  {
    name: "files.file_transfer",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const local = await localDir(ctx);
      const bytes = randomBytes(5 * MiB);
      await writeFile(join(local, "up.bin"), bytes);
      const remote = `/tmp/${ctx.name("transfer")}.bin`;
      await sandbox.files.upload(join(local, "up.bin"), remote);
      assert.equal((await sandbox.files.stat(remote)).size, bytes.length);
      await sandbox.files.download(remote, join(local, "down.bin"));
      assert.ok((await readFile(join(local, "down.bin"))).equals(bytes), "a file transfer round-trips every byte");
    },
  },
  {
    name: "files.dir_transfer",
    run: async (ctx) => {
      const sandbox = await ctx.sandbox();
      const local = await localDir(ctx);
      const source = join(local, "source");
      await mkdir(join(source, "nested", "deeper"), { recursive: true });
      await writeFile(join(source, "top.txt"), "top\n");
      await writeFile(join(source, "nested", "mid.bin"), randomBytes(MiB));
      await writeFile(join(source, "nested", "deeper", "leaf.txt"), "leaf\n");
      await writeFile(join(source, "nested", "empty"), "");
      const remote = `/tmp/${ctx.name("dir")}`;
      await sandbox.files.uploadDir(source, remote);
      assert.equal(await sandbox.files.read(`${remote}/nested/deeper/leaf.txt`, { encoding: "utf8" }), "leaf\n");
      const back = join(local, "back");
      await sandbox.files.downloadDir(remote, back);
      const want = await tree(source);
      const got = await tree(back);
      assert.deepEqual([...got.keys()].sort(), [...want.keys()].sort(), "the same files come back");
      for (const [path, bytes] of want) {
        assert.ok(got.get(path)?.equals(bytes), `${path} came back changed`);
      }
      assert.ok((await stat(join(back, "nested", "deeper"))).isDirectory());
    },
  },
];
