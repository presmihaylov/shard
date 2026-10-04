// The tar format, as much of it as a directory transfer needs: ustar headers it writes, and PAX and GNU long names it reads too.
import { ProtocolError } from "./errors.js";

export const blockSize = 512;
// A PAX or GNU long-name record past this is no header a sandbox writes, as Go's reader caps it.
const specialLimit = 1 << 20;

export const typeFile = "0";
export const typeHardlink = "1";
export const typeSymlink = "2";
export const typeChar = "3";
export const typeBlock = "4";
export const typeDir = "5";
export const typeFifo = "6";
export const typeGlobal = "g";
const typePax = "x";
const typeLongName = "L";
const typeLongLink = "K";

/** Header is one entry's header; mtime is in seconds. */
export interface Header {
  name: string;
  mode: number;
  uid: number;
  gid: number;
  size: number;
  mtime: number;
  type: string;
  linkname: string;
}

/** end is the two zero blocks that close an archive. */
export const end = Buffer.alloc(2 * blockSize);

/** encodeHeader answers the ustar header of an entry, behind a PAX header for any field ustar cannot hold. */
export function encodeHeader(header: Header): Buffer {
  const records: Array<[string, string]> = [];
  if (Buffer.byteLength(header.name) > 100) {
    records.push(["path", header.name]);
  }
  if (Buffer.byteLength(header.linkname) > 100) {
    records.push(["linkpath", header.linkname]);
  }
  const numbers: Array<[string, number, number]> = [
    ["size", header.size, 12],
    ["uid", header.uid, 8],
    ["gid", header.gid, 8],
    ["mtime", header.mtime, 12],
  ];
  for (const [key, value, width] of numbers) {
    if (!fits(value, width)) {
      records.push([key, String(value)]);
    }
  }
  const block = ustar(header);
  if (records.length === 0) {
    return block;
  }
  const body = Buffer.concat(records.map(([key, value]) => paxRecord(key, value)));
  const pax = ustar({ name: `PaxHeaders/${header.name.split("/").filter(Boolean).pop() ?? ""}`, mode: 0o644, uid: 0, gid: 0, size: body.length, mtime: 0, type: typePax, linkname: "" });

  return Buffer.concat([pax, body, padding(body.length), block]);
}

/** padding answers the zero bytes that fill an entry's data of size bytes up to a whole block. */
export function padding(size: number): Buffer {
  return Buffer.alloc((blockSize - (size % blockSize)) % blockSize);
}

function ustar(header: Header): Buffer {
  const block = Buffer.alloc(blockSize);
  Buffer.from(header.name).copy(block, 0, 0, 100);
  octal(block, 100, 8, header.mode);
  octal(block, 108, 8, header.uid);
  octal(block, 116, 8, header.gid);
  octal(block, 124, 12, header.size);
  octal(block, 136, 12, header.mtime);
  block.write(header.type, 156, "latin1");
  Buffer.from(header.linkname).copy(block, 157, 0, 100);
  block.write("ustar\u000000", 257, "latin1");
  block.write(checksums(block).unsigned.toString(8).padStart(6, "0") + "\u0000 ", 148, "latin1");

  return block;
}

/** octal writes a value that fits as zero-padded octal digits; one that does not is left 0, for the PAX header to carry. */
function octal(block: Buffer, at: number, width: number, value: number): void {
  block.write((fits(value, width) ? value : 0).toString(8).padStart(width - 1, "0"), at, width - 1, "latin1");
}

function fits(value: number, width: number): boolean {
  return Number.isSafeInteger(value) && value >= 0 && value < 8 ** (width - 1);
}

/** paxRecord answers "<length> <key>=<value>\n", where the length counts its own digits. */
function paxRecord(key: string, value: string): Buffer {
  const rest = Buffer.byteLength(` ${key}=${value}\n`);
  let length = rest + 1;
  while (String(length).length + rest !== length) {
    length = String(length).length + rest;
  }

  return Buffer.from(`${length} ${key}=${value}\n`);
}

function checksums(block: Buffer): { unsigned: number; signed: number } {
  let unsigned = 0;
  let signed = 0;
  for (const [i, byte] of block.entries()) {
    const b = i >= 148 && i < 156 ? 0x20 : byte;
    unsigned += b;
    signed += b > 127 ? b - 256 : b;
  }

  return { unsigned, signed };
}

/** TarReader reads entries off a byte stream, one header and then its data at a time. */
export class TarReader {
  private readonly source: AsyncIterator<Uint8Array>;
  private held: Buffer = Buffer.alloc(0);
  private ended = false;
  private remaining = 0;
  private pad = 0;
  private current = "";

  constructor(source: AsyncIterable<Uint8Array>) {
    this.source = source[Symbol.asyncIterator]();
  }

  /** next answers the next entry's header, or undefined at the end; any data of the entry before is skipped. */
  async next(): Promise<Header | undefined> {
    await this.skip(this.remaining + this.pad);
    this.remaining = 0;
    this.pad = 0;
    const extended = new Map<string, string>();
    for (;;) {
      const block = await this.exactly(blockSize, "a header");
      if (block === undefined) {
        return undefined;
      }
      if (block.every((b) => b === 0)) {
        return this.closing();
      }
      const header = parse(block);
      if (await this.extend(header, extended)) {
        continue;
      }

      return this.begin(header, extended);
    }
  }

  /** data yields the current entry's data, exactly its size. */
  async *data(): AsyncGenerator<Buffer> {
    while (this.remaining > 0) {
      const chunk = await this.some(this.remaining);
      if (chunk.length === 0) {
        throw new ProtocolError(`the tar ends inside the data of ${JSON.stringify(this.current)}`);
      }
      this.remaining -= chunk.length;
      yield chunk;
    }
  }

  private begin(header: Header, extended: Map<string, string>): Header {
    const name = extended.get("path") ?? header.name;
    const linkname = extended.get("linkpath") ?? header.linkname;
    if (name.includes("\0") || linkname.includes("\0")) {
      throw new ProtocolError(`the tar names an entry with a NUL in it: ${JSON.stringify(name)}`);
    }
    const size = paxNumber(extended, "size", name) ?? header.size;
    const uid = paxNumber(extended, "uid", name) ?? header.uid;
    const gid = paxNumber(extended, "gid", name) ?? header.gid;
    const mtime = paxNumber(extended, "mtime", name) ?? header.mtime;
    // An old tar marks a regular file 0 or NUL, and a directory by its trailing slash alone.
    const legacy = header.type === "\0";
    const type = legacy && name.endsWith("/") ? typeDir : legacy ? typeFile : header.type;
    // These types have no data, whatever the size field says, as Go's reader reads them.
    const headerOnly = [typeHardlink, typeSymlink, typeChar, typeBlock, typeDir, typeFifo].includes(type);
    this.remaining = headerOnly ? 0 : size;
    this.pad = padding(this.remaining).length;
    this.current = name;

    return { ...header, name, linkname, size, uid, gid, mtime, type };
  }

  /** extend merges a PAX or GNU long name header into the fields of the entry after it, and answers false for any other header. */
  private async extend(header: Header, extended: Map<string, string>): Promise<boolean> {
    if (header.type !== typePax && header.type !== typeLongName && header.type !== typeLongLink) {
      return false;
    }
    const body = await this.special(header);
    if (header.type !== typePax) {
      extended.set(header.type === typeLongName ? "path" : "linkpath", cstring(body, 0, body.length));

      return true;
    }
    for (const [key, value] of paxRecords(body)) {
      extended.set(key, value);
    }

    return true;
  }

  /** closing answers the end after the first zero block, which a second one, or the end of the stream, confirms. */
  private async closing(): Promise<undefined> {
    const second = await this.exactly(blockSize, "the end of the archive");
    if (second !== undefined && !second.every((b) => b === 0)) {
      throw new ProtocolError("the tar holds a header after a zero block");
    }

    return undefined;
  }

  private async special(header: Header): Promise<Buffer> {
    if (header.size > specialLimit) {
      throw new ProtocolError(`the tar holds a ${header.size} byte extended header, past the ${specialLimit} the SDK reads`);
    }
    const body = await this.exactly(header.size, "an extended header");
    await this.skip(padding(header.size).length);

    return body ?? Buffer.alloc(0);
  }

  /** exactly answers n bytes, or undefined when the stream ended before the first; an end inside them is a cut tar. */
  private async exactly(n: number, what: string): Promise<Buffer | undefined> {
    while (this.held.length < n && !this.ended) {
      await this.pull();
    }
    if (this.held.length === 0 && n > 0) {
      return undefined;
    }
    if (this.held.length < n) {
      throw new ProtocolError(`the tar ends inside ${what}`);
    }
    const out = this.held.subarray(0, n);
    this.held = this.held.subarray(n);

    return out;
  }

  /** some answers up to n bytes, at least one unless the stream ended. */
  private async some(n: number): Promise<Buffer> {
    if (this.held.length === 0 && !this.ended) {
      await this.pull();
    }
    const out = this.held.subarray(0, n);
    this.held = this.held.subarray(out.length);

    return out;
  }

  private async skip(n: number): Promise<void> {
    for (let left = n; left > 0; ) {
      const chunk = await this.some(left);
      if (chunk.length === 0) {
        throw new ProtocolError(`the tar ends inside the data of ${JSON.stringify(this.current)}`);
      }
      left -= chunk.length;
    }
  }

  private async pull(): Promise<void> {
    for (;;) {
      const { done, value } = await this.source.next();
      if (done) {
        this.ended = true;

        return;
      }
      if (value.length === 0) {
        continue;
      }
      const chunk = Buffer.from(value.buffer, value.byteOffset, value.byteLength);
      this.held = this.held.length === 0 ? chunk : Buffer.concat([this.held, chunk]);

      return;
    }
  }
}

/** paxNumber is a PAX record's number, in whole seconds for a time, or undefined when the tar has no such record. */
function paxNumber(extended: Map<string, string>, key: string, name: string): number | undefined {
  const value = extended.get(key);
  if (value === undefined) {
    return undefined;
  }
  const number = Math.floor(Number(value));
  const pattern = key === "mtime" ? /^-?\d+(\.\d+)?$/ : /^\d+$/;
  if (!pattern.test(value) || !Number.isSafeInteger(number)) {
    throw new ProtocolError(`the tar gives ${JSON.stringify(name)} the ${key} ${JSON.stringify(value)}`);
  }

  return number;
}

function parse(block: Buffer): Header {
  const field = numeric(block, 148, 8);
  const { unsigned, signed } = checksums(block);
  if (field !== unsigned && field !== signed) {
    throw new ProtocolError("the tar holds a header whose checksum does not match");
  }
  const magic = block.toString("latin1", 257, 265);
  const name = cstring(block, 0, 100);
  // Only POSIX ustar has the prefix; GNU keeps other fields there.
  const prefix = magic === "ustar\u000000" ? cstring(block, 345, 155) : "";
  const size = numeric(block, 124, 12);
  if (size < 0) {
    throw new ProtocolError(`the tar gives ${JSON.stringify(name)} the size ${size}`);
  }

  return {
    name: prefix ? `${prefix}/${name}` : name,
    mode: numeric(block, 100, 8),
    uid: numeric(block, 108, 8),
    gid: numeric(block, 116, 8),
    size,
    mtime: numeric(block, 136, 12),
    type: String.fromCharCode(block[156] ?? 0),
    linkname: cstring(block, 157, 100),
  };
}

/** numeric reads an octal field, or GNU's base-256 one when the high bit of its first byte is set. */
function numeric(block: Buffer, at: number, width: number): number {
  const first = block[at] ?? 0;
  if (first & 0x80) {
    let value = BigInt(first & 0x3f);
    for (const byte of block.subarray(at + 1, at + width)) {
      value = (value << 8n) | BigInt(byte);
    }
    const signed = first & 0x40 ? value - (1n << BigInt(8 * width - 2)) : value;
    if (signed > BigInt(Number.MAX_SAFE_INTEGER) || signed < BigInt(Number.MIN_SAFE_INTEGER)) {
      throw new ProtocolError(`the tar holds a number past what the SDK reads, at offset ${at}`);
    }

    return Number(signed);
  }
  const digits = block.toString("latin1", at, at + width).replace(/^[ \0]+|[ \0]+$/g, "");
  if (!/^[0-7]*$/.test(digits)) {
    throw new ProtocolError(`the tar holds a header with ${JSON.stringify(digits)} where a number goes, at offset ${at}`);
  }

  return digits ? Number.parseInt(digits, 8) : 0;
}

function cstring(block: Buffer, at: number, width: number): string {
  const field = block.subarray(at, at + width);
  const nul = field.indexOf(0);

  return field.subarray(0, nul < 0 ? field.length : nul).toString("utf8");
}

function paxRecords(body: Buffer): Array<[string, string]> {
  const records: Array<[string, string]> = [];
  for (let at = 0; at < body.length; ) {
    const space = body.indexOf(0x20, at);
    const length = Number(body.toString("latin1", at, space));
    const record = body.subarray(at, at + length);
    const text = record.toString("utf8", space - at + 1, record.length - 1);
    const equals = text.indexOf("=");
    if (space < 0 || !Number.isSafeInteger(length) || length <= space - at || at + length > body.length || record[record.length - 1] !== 0x0a || equals < 1) {
      throw new ProtocolError("the tar holds a PAX header the SDK cannot read");
    }
    records.push([text.slice(0, equals), text.slice(equals + 1)]);
    at += length;
  }

  return records;
}
