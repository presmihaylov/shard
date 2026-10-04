// Pack a local directory as a tar, and unpack a sandbox's tar under a directory it never writes outside of.
import { constants, type BigIntStats, type Stats } from "node:fs";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { UnknownLengthError, UnsafeArchiveError } from "./errors.js";
import * as tar from "./tar.js";

// The Go client's caps, since a sandbox's tar is untrusted.
export const maxBytes = 64 * 2 ** 30;
export const maxEntries = 2 ** 20;
const chunkSize = 1 << 20;
// The links a path may pass through before it counts as a loop, as Linux counts them.
const maxHops = 40;

/** pack yields source as a tar whose top entry is name, a directory's entries in lexical order. */
export async function* pack(source: string, name: string): AsyncGenerator<Buffer> {
  // The walk starts past a link the source names, as an open of a file follows one.
  const top = await fs.realpath(source);
  yield* entry(top, name, await fs.lstat(top));
  yield tar.end;
}

/** entry yields one entry: a socket has no tar type and stays out, and a fifo or a device goes in as a header alone. */
async function* entry(at: string, name: string, info: Stats): AsyncGenerator<Buffer> {
  const header: tar.Header = {
    name,
    mode: info.mode & 0o7777,
    uid: info.uid,
    gid: info.gid,
    size: 0,
    mtime: Math.floor(info.mtimeMs / 1000),
    type: tar.typeFile,
    linkname: "",
  };
  if (info.isDirectory()) {
    yield tar.encodeHeader({ ...header, name: `${name}/`, type: tar.typeDir });
    yield* children(at, name);

    return;
  }
  if (info.isSymbolicLink()) {
    yield tar.encodeHeader({ ...header, type: tar.typeSymlink, linkname: await fs.readlink(at) });

    return;
  }
  // The sandbox's unpack refuses every device, so no device number ever lands and none is sent.
  const special = info.isFIFO() ? tar.typeFifo : info.isCharacterDevice() ? tar.typeChar : info.isBlockDevice() ? tar.typeBlock : undefined;
  if (special) {
    yield tar.encodeHeader({ ...header, type: special });

    return;
  }
  if (info.isFile()) {
    yield* file(at, { ...header, size: info.size });
  }
}

async function* children(at: string, name: string): AsyncGenerator<Buffer> {
  const names = (await fs.readdir(at)).sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
  for (const child of names) {
    const info = await fs.lstat(path.join(at, child)).catch((err: unknown) => {
      // A name removed between the directory read and its visit is gone, which is what a later walk would say too.
      if (isCode(err, "ENOENT")) {
        return undefined;
      }
      throw err;
    });
    if (info) {
      yield* entry(path.join(at, child), `${name}/${child}`, info);
    }
  }
}

/** file copies exactly the size the header names from a file opened without following a link, so a file that changes under the walk fails and never lies. */
async function* file(at: string, header: tar.Header): AsyncGenerator<Buffer> {
  const handle = await fs.open(at, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    yield tar.encodeHeader(header);
    for (let left = header.size; left > 0; ) {
      const want = Math.min(chunkSize, left);
      const { bytesRead, buffer } = await handle.read(Buffer.alloc(want), 0, want, null);
      if (bytesRead === 0) {
        throw new UnknownLengthError(`pack ${at}: the file ended ${left} bytes short of the ${header.size} it held at the start`);
      }
      left -= bytesRead;
      yield buffer.subarray(0, bytesRead);
    }
    yield tar.padding(header.size);
  } finally {
    await handle.close();
  }
}

/** unpack lands the tar under dst, the entry named strip at dst itself, by the rules of the Go client's unpack. */
export async function unpack(source: AsyncIterable<Uint8Array>, dst: string, strip: string): Promise<void> {
  await new Unpacker(path.resolve(dst), await fs.realpath(dst), strip).run(new tar.TarReader(source));
}

class Unpacker {
  private bytes = 0;
  private temps = 0;
  // The regular files this unpack wrote, by identity, the only targets a hard link may name.
  private readonly files = new Map<string, BigIntStats>();
  private readonly dirs: Array<[string, number]> = [];
  private links: Array<[string, string]> = [];

  constructor(
    private readonly dst: string,
    private readonly real: string,
    private readonly strip: string,
  ) {}

  async run(reader: tar.TarReader): Promise<void> {
    try {
      await this.entries(reader);
    } catch (err) {
      // A refused unpack leaves no link behind that leaves dst either.
      await this.confine().catch((cleanup: unknown) => {
        throw new AggregateError([err, cleanup], "the unpack failed, and so did the removal of a link that leaves the destination");
      });
      throw err;
    }
    const [refused] = await this.confine();
    if (refused) {
      throw refused;
    }
    // Deepest first, so each directory's entries landed whatever its own mode is.
    for (const [at, mode] of this.dirs.reverse()) {
      await fs.chmod(at, mode);
    }
  }

  private async entries(reader: tar.TarReader): Promise<void> {
    for (let count = 0; ; count++) {
      const header = await reader.next();
      if (!header) {
        return;
      }
      if (count >= maxEntries) {
        throw new UnsafeArchiveError(header.name, `the archive runs past ${maxEntries} entries`);
      }
      await this.entry(header, reader);
    }
  }

  private async entry(header: tar.Header, reader: tar.TarReader): Promise<void> {
    if (header.type === tar.typeGlobal) {
      return;
    }
    const name = this.local(header.name);
    if (name === "." && header.type !== tar.typeDir) {
      throw new UnsafeArchiveError(header.name, "only a directory can land at the destination itself");
    }
    switch (header.type) {
      case tar.typeDir:
        return this.dir(name, header);
      case tar.typeFile:
        return this.file(name, header, reader);
      case tar.typeSymlink:
        return this.symlink(name, header);
      case tar.typeHardlink:
        return this.hardlink(name, header);
      case tar.typeChar:
      case tar.typeBlock:
        throw new UnsafeArchiveError(header.name, "a device node");
      case tar.typeFifo:
        throw new UnsafeArchiveError(header.name, "a fifo, which an unpack does not make");
      default:
        throw new UnsafeArchiveError(header.name, `the type '${header.type}', which an unpack does not make`);
    }
  }

  /** local is the entry's name under dst: no absolute name and no .. before the clean, then strip cut off. */
  private local(name: string): string {
    if (!name) {
      throw new UnsafeArchiveError(name, "an empty name");
    }
    if (name.startsWith("/")) {
      throw new UnsafeArchiveError(name, "an absolute name");
    }
    if (name.split("/").includes("..")) {
      throw new UnsafeArchiveError(name, "a .. component");
    }
    const clean = path.posix.normalize(name).replace(/\/$/, "");
    if (clean === this.strip) {
      return ".";
    }
    if (!clean.startsWith(`${this.strip}/`)) {
      throw new UnsafeArchiveError(name, `it is not under ${this.strip}`);
    }

    return clean.slice(this.strip.length + 1);
  }

  /** parent makes name's directories as mkdir -p does, through no symlink, and answers the path name lands at. */
  private async parent(name: string, entry: string): Promise<string> {
    if (name === ".") {
      return this.dst;
    }
    const parts = name.split("/");
    let at = this.dst;
    for (const part of parts.slice(0, -1)) {
      at = path.join(at, part);
      const info = await lstatOf(at);
      if (!info) {
        await fs.mkdir(at, 0o755);
        continue;
      }
      if (info.isSymbolicLink()) {
        throw new UnsafeArchiveError(entry, "it sits under a symlink, which an unpack never writes through");
      }
    }
    const target = path.join(this.dst, ...parts);
    // A name the platform reads otherwise, as a backslash on Windows, must still land under dst.
    if (!inside(this.dst, target)) {
      throw new UnsafeArchiveError(entry, "it leaves the destination");
    }

    return target;
  }

  private async dir(name: string, header: tar.Header): Promise<void> {
    const at = await this.parent(name, header.name);
    try {
      // Only its owner can write it until the tree is in, so its entries land whatever its mode.
      await fs.mkdir(at, 0o700);
    } catch (err) {
      if (!isCode(err, "EEXIST")) {
        throw err;
      }
      if (!(await fs.lstat(at)).isDirectory()) {
        throw new UnsafeArchiveError(header.name, "a directory where something else already is");
      }

      return;
    }
    this.dirs.push([at, modeOf(header)]);
  }

  private async file(name: string, header: tar.Header, reader: tar.TarReader): Promise<void> {
    if (header.size > maxBytes - this.bytes) {
      throw new UnsafeArchiveError(header.name, `the archive runs past ${maxBytes} bytes`);
    }
    this.bytes += header.size;
    const at = await this.parent(name, header.name);
    const tmp = this.temp(at);
    // The open comes first, so a failed write never races the creation of the temp it removes.
    const handle = await fs.open(tmp, "wx", 0o600);
    try {
      await fs.writeFile(handle, reader.data()).finally(() => handle.close());
      await fs.chmod(tmp, modeOf(header));
    } catch (err) {
      await removeTemp(tmp);
      throw err;
    }
    await this.place(tmp, at, name, true);
  }

  private async symlink(name: string, header: tar.Header): Promise<void> {
    if (leaves(name, header.linkname)) {
      throw new UnsafeArchiveError(header.name, `a symlink to ${JSON.stringify(header.linkname)}, which leaves the destination`);
    }
    const at = await this.parent(name, header.name);
    const tmp = this.temp(at);
    await fs.symlink(header.linkname, tmp);
    this.links.push([at, header.name]);
    await this.place(tmp, at, name, false);
  }

  private async hardlink(name: string, header: tar.Header): Promise<void> {
    const refusal = new UnsafeArchiveError(header.name, `a hard link to ${JSON.stringify(header.linkname)}, which is not a file earlier in the archive`);
    let target: string;
    try {
      target = this.local(header.linkname);
    } catch {
      throw refusal;
    }
    const wrote = this.files.get(target);
    if (!wrote) {
      throw refusal;
    }
    const at = await this.parent(name, header.name);
    const tmp = this.temp(at);
    await fs.link(path.join(this.dst, ...target.split("/")), tmp);
    // A filesystem that folds case lets a later entry spelled another way take the name.
    const linked = await fs.lstat(tmp, { bigint: true });
    if (!linked.isFile() || linked.dev !== wrote.dev || linked.ino !== wrote.ino) {
      await removeTemp(tmp);
      throw new UnsafeArchiveError(header.name, `a hard link to ${JSON.stringify(header.linkname)}, which no longer names the file the archive wrote`);
    }
    await this.place(tmp, at, name, true);
  }

  /** place renames the finished temp over the name, so a link already there is replaced and never followed. */
  private async place(tmp: string, at: string, name: string, isFile: boolean): Promise<void> {
    try {
      await fs.rename(tmp, at);
    } catch (err) {
      await removeTemp(tmp);
      throw err;
    }
    if (!isFile) {
      this.files.delete(name);

      return;
    }
    this.files.set(name, await fs.lstat(at, { bigint: true }));
  }

  private temp(at: string): string {
    this.temps++;

    return path.join(path.dirname(at), `.useshards-unpack-${process.pid}-${this.temps}`);
  }

  /** confine removes every new link that leaves dst through another one, which only the tree as written shows. */
  private async confine(): Promise<UnsafeArchiveError[]> {
    const refused: UnsafeArchiveError[] = [];
    for (const [at, entry] of this.links.splice(0)) {
      if (inside(this.real, await resolved(at))) {
        continue;
      }
      refused.push(new UnsafeArchiveError(entry, "a symlink that leaves the destination through another link"));
      await removeTemp(at);
    }

    return refused;
  }
}

/** leaves is the lexical half of the link check: an absolute target, or a .. that climbs past dst from where the link sits. */
function leaves(name: string, target: string): boolean {
  if (path.posix.isAbsolute(target)) {
    return true;
  }
  const at = path.posix.join(path.posix.dirname(name), target);

  return at === ".." || at.startsWith("../");
}

/** resolved follows every link on the path that exists and keeps the rest as written, as Python's non-strict realpath does. */
async function resolved(at: string): Promise<string> {
  const pending = at.split(path.sep).filter(Boolean);
  let done = path.parse(at).root;
  for (let hops = 0; pending.length > 0; ) {
    const part = pending.shift() ?? "";
    if (part === ".") {
      continue;
    }
    if (part === "..") {
      done = path.dirname(done);
      continue;
    }
    const next = path.join(done, part);
    const link = await linkOf(next);
    if (link === undefined) {
      done = next;
      continue;
    }
    // A loop resolves no further, so it stays where it already is.
    if (++hops > maxHops) {
      return path.join(next, ...pending);
    }
    pending.unshift(...link.split(path.sep).filter(Boolean));
    if (path.isAbsolute(link)) {
      done = path.parse(link).root;
    }
  }

  return done;
}

async function linkOf(at: string): Promise<string | undefined> {
  try {
    return await fs.readlink(at);
  } catch (err) {
    // Not a link, or not there: the path goes on as written.
    if (isCode(err, "EINVAL") || isCode(err, "ENOENT") || isCode(err, "ENOTDIR")) {
      return undefined;
    }
    throw err;
  }
}

function inside(root: string, at: string): boolean {
  const rel = path.relative(root, at);

  return rel !== ".." && !rel.startsWith(`..${path.sep}`) && !path.isAbsolute(rel);
}

async function lstatOf(at: string): Promise<Stats | undefined> {
  try {
    return await fs.lstat(at);
  } catch (err) {
    if (isCode(err, "ENOENT")) {
      return undefined;
    }
    throw err;
  }
}

/** modeOf is the tar's mode with setuid and setgid dropped, so a sandbox cannot hand this host a set-id binary. */
function modeOf(header: tar.Header): number {
  return header.mode & (0o777 | 0o1000);
}

async function removeTemp(at: string): Promise<void> {
  await fs.rm(at, { force: true });
}

export function isCode(err: unknown, code: string): boolean {
  return err instanceof Error && "code" in err && err.code === code;
}
