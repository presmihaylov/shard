import assert from "node:assert/strict";
import { test } from "node:test";
import { ProtocolError } from "../src/errors.js";
import * as tar from "../src/tar.js";

const header = (fields: Partial<tar.Header> & { name: string }): tar.Header => ({
  mode: 0o644,
  uid: 0,
  gid: 0,
  size: 0,
  mtime: 0,
  type: tar.typeFile,
  linkname: "",
  ...fields,
});

function file(name: string, data: string, fields: Partial<tar.Header> = {}): Buffer {
  const body = Buffer.from(data);

  return Buffer.concat([tar.encodeHeader(header({ name, size: body.length, ...fields })), body, tar.padding(body.length)]);
}

// Small chunks, so every read of the reader spans a chunk boundary.
async function* chunks(buffer: Buffer, size = 97): AsyncGenerator<Uint8Array> {
  for (let at = 0; at < buffer.length; at += size) {
    yield buffer.subarray(at, at + size);
  }
}

async function read(buffer: Buffer): Promise<Array<{ header: tar.Header; data: string }>> {
  const reader = new tar.TarReader(chunks(buffer));
  const entries: Array<{ header: tar.Header; data: string }> = [];
  for (let next = await reader.next(); next; next = await reader.next()) {
    const parts: Buffer[] = [];
    for await (const part of reader.data()) {
      parts.push(part);
    }
    entries.push({ header: next, data: Buffer.concat(parts).toString() });
  }

  return entries;
}

test("a header round-trips its fields", async () => {
  const [entry] = await read(Buffer.concat([file("top/a.txt", "hello", { mode: 0o4755, uid: 1000, gid: 1001, mtime: 1_700_000_000 }), tar.end]));
  assert.deepEqual(entry, {
    header: header({ name: "top/a.txt", mode: 0o4755, uid: 1000, gid: 1001, size: 5, mtime: 1_700_000_000 }),
    data: "hello",
  });
});

test("a name or a link past 100 bytes, and a number past its field, go through a PAX header", async () => {
  const long = `top/${"d".repeat(120)}/${"f".repeat(150)}`;
  const target = `../${"t".repeat(200)}`;
  const archive = Buffer.concat([
    file(long, "x", { uid: 8 ** 7, mtime: 8 ** 11 }),
    tar.encodeHeader(header({ name: "top/link", type: tar.typeSymlink, linkname: target })),
    tar.end,
  ]);
  const entries = await read(archive);
  assert.deepEqual(
    entries.map((entry) => [entry.header.name, entry.header.linkname, entry.header.uid, entry.header.mtime, entry.data]),
    [
      [long, "", 8 ** 7, 8 ** 11, "x"],
      ["top/link", target, 0, 0, ""],
    ],
  );
});

test("a reader skips the data of an entry its caller never read", async () => {
  const archive = Buffer.concat([file("a", "x".repeat(1500)), file("b", "second"), tar.end]);
  const reader = new tar.TarReader(chunks(archive));
  assert.equal((await reader.next())?.name, "a");
  assert.equal((await reader.next())?.name, "b");
  const parts: Buffer[] = [];
  for await (const part of reader.data()) {
    parts.push(part);
  }
  assert.equal(Buffer.concat(parts).toString(), "second");
  assert.equal(await reader.next(), undefined);
});

test("a GNU long name and the ustar prefix name the entry", async () => {
  const long = "n".repeat(130);
  const gnu = Buffer.concat([file("././@LongLink", `${long}\0`, { type: "L" }), file("short", "g")]);
  const prefixed = tar.encodeHeader(header({ name: "leaf", size: 1 }));
  prefixed.write("some/prefix", 345, "latin1");
  writeChecksum(prefixed);
  const archive = Buffer.concat([gnu, prefixed, Buffer.from("p"), tar.padding(1), tar.end]);
  assert.deepEqual(
    (await read(archive)).map((entry) => [entry.header.name, entry.data]),
    [
      [long, "g"],
      ["some/prefix/leaf", "p"],
    ],
  );
});

test("a size in GNU base-256 is read", async () => {
  const block = tar.encodeHeader(header({ name: "big", size: 3 }));
  block.fill(0, 124, 136);
  block[124] = 0x80;
  block[135] = 3;
  writeChecksum(block);
  const [entry] = await read(Buffer.concat([block, Buffer.from("abc"), tar.padding(3), tar.end]));
  assert.equal(entry?.data, "abc");
});

// A negative size would lower the byte count an extract holds against its limit.
test("a negative size in GNU base-256 is refused", async () => {
  const block = tar.encodeHeader(header({ name: "neg", size: 0 }));
  block.fill(0xff, 124, 136);
  writeChecksum(block);
  await assert.rejects(read(Buffer.concat([block, tar.end])), (err) => err instanceof ProtocolError && /"neg" the size -1/.test(err.message));
});

test("a negative mtime in GNU base-256 is read", async () => {
  const block = tar.encodeHeader(header({ name: "old", size: 0 }));
  block.fill(0xff, 136, 148);
  writeChecksum(block);
  const [entry] = await read(Buffer.concat([block, tar.end]));
  assert.equal(entry?.header.mtime, -1);
});

test("a tar with no end blocks ends at its last entry", async () => {
  assert.deepEqual(
    (await read(file("only", "x"))).map((entry) => entry.header.name),
    ["only"],
  );
});

test("a header whose checksum does not match is refused", async () => {
  const archive = file("a", "x");
  archive[0] = "b".charCodeAt(0);
  await assert.rejects(read(archive), (err) => err instanceof ProtocolError && /checksum/.test(err.message));
});

test("a tar that ends inside an entry's data is refused", async () => {
  const archive = file("a", "x".repeat(600)).subarray(0, 700);
  await assert.rejects(read(archive), (err) => err instanceof ProtocolError && /ends inside the data of "a"/.test(err.message));
});

test("a header after one zero block is refused", async () => {
  const archive = Buffer.concat([file("a", "x"), Buffer.alloc(tar.blockSize), file("b", "y")]);
  await assert.rejects(read(archive), (err) => err instanceof ProtocolError && /after a zero block/.test(err.message));
});

test("a name with a NUL in a PAX path is refused", async () => {
  const record = (key: string, value: string): string => {
    const body = ` ${key}=${value}\n`;
    let length = body.length + 1;
    while (`${length}${body}`.length !== length) {
      length++;
    }

    return `${length}${body}`;
  };
  const archive = Buffer.concat([file("pax", record("path", "a\0b"), { type: "x" }), file("a", "x"), tar.end]);
  await assert.rejects(read(archive), (err) => err instanceof ProtocolError && /NUL/.test(err.message));
});

test("padding fills an entry to a whole block", () => {
  assert.deepEqual(
    [0, 1, 511, 512, 513].map((size) => tar.padding(size).length),
    [0, 511, 1, 0, 511],
  );
});

function writeChecksum(block: Buffer): void {
  block.fill(" ", 148, 156);
  let sum = 0;
  for (const byte of block) {
    sum += byte;
  }
  block.write(`${sum.toString(8).padStart(6, "0")}\0 `, 148, "latin1");
}
