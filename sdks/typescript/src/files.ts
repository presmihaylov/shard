// The files of one sandbox: a sandbox path is absolute, a local path is on this host.
import { randomBytes } from "node:crypto";
import { createReadStream } from "node:fs";
import * as fs from "node:fs/promises";
import type { IncomingHttpHeaders } from "node:http";
import * as os from "node:os";
import * as path from "node:path";
import { isCode, pack, unpack } from "./archive.js";
import { Fields } from "./decode.js";
import { ProtocolError, UnknownLengthError } from "./errors.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";

const statHeader = "x-shard-stat";
const chunkSize = 1 << 20;

export type FileType = "file" | "dir" | "symlink" | "other";

const fileTypes: readonly FileType[] = ["file", "dir", "symlink", "other"];

/** FileInfo is one sandbox path as the guest sees it; mode is the permission bits with setuid, setgid and sticky. */
export interface FileInfo {
  type: FileType;
  size: number;
  mode: number;
  uid: number;
  gid: number;
  mtime: Date;
}

/** FileEntry is one name in a sandbox directory with its own stat, never what a symlink points to. */
export interface FileEntry extends FileInfo {
  name: string;
}

export interface WriteOptions {
  /** The permission bits, as 0o644. */
  mode?: number;
  /** Makes the missing parent directories. */
  parents?: boolean;
  /** A user name or uid in the sandbox, which owns what the call makes. */
  user?: string;
}

type Route = "files" | "ls" | "mkdir" | "archive";

export class Files {
  constructor(
    private readonly transport: Transport,
    private readonly sandboxId: string,
  ) {}

  /** copy a file out of a running sandbox */
  read(path: string): Promise<Uint8Array>;
  read(path: string, options: { encoding: "utf8" }): Promise<string>;
  async read(path: string, options?: { encoding: "utf8" }): Promise<Uint8Array | string> {
    const { body } = await this.transport.fetch("GET", this.route("files"), { query: { path } });
    if (options?.encoding === "utf8") {
      return body.toString("utf8");
    }

    return body;
  }

  /** copy a file into a running sandbox */
  async write(path: string, data: string | Uint8Array, options: WriteOptions = {}): Promise<void> {
    const body = typeof data === "string" ? Buffer.from(data) : data;
    await this.put(path, body, body.length, options);
  }

  async stat(path: string): Promise<FileInfo> {
    return statOf(await this.transport.head(this.route("files"), `a stat of ${path}`, { query: { path } }), `stat ${path}`);
  }

  /** list answers the entries of the directory at path, each with its own stat. */
  async list(path: string): Promise<FileEntry[]> {
    const { body } = await this.transport.fetch("GET", this.route("ls"), { query: { path } });
    let listing: unknown;
    try {
      listing = JSON.parse(body.toString("utf8"));
    } catch {
      // The daemon closes the JSON only once the guest sent every entry, so a cut listing never parses.
      throw new ProtocolError(`the daemon cut the listing of ${path} short`);
    }

    return Fields.of(listing, `a listing of ${path}`)
      .list("entries")
      .map((entry) => ({ name: entry.string("name"), ...fileInfo(entry) }));
  }

  async mkdir(path: string, options: WriteOptions = {}): Promise<void> {
    const { mode, parents, user } = options;
    const body = { path, mode: mode?.toString(8), parents, user };
    await this.transport.api.POST("/v0/sandboxes/{id}/mkdir", { params: { path: { id: this.sandboxId } }, body });
  }

  async remove(path: string, options: { recursive?: boolean } = {}): Promise<void> {
    const params = { path: { id: this.sandboxId }, query: { path, recursive: options.recursive || undefined } };
    // A recursive remove waits on the guest for as long as the tree takes, so no bound cuts it.
    await this.transport.api.DELETE("/v0/sandboxes/{id}/files", { params, fetch: this.transport.waiting });
  }

  /** copy a file into a running sandbox */
  async upload(local: string, remote: string, options: WriteOptions = {}): Promise<void> {
    const handle = await fs.open(local, "r");
    try {
      const info = await handle.stat();
      if (!info.isFile()) {
        throw new UnknownLengthError(`upload ${local}: not a regular file, so its length is not known up front`);
      }
      const mode = options.mode ?? info.mode & 0o777;
      await this.put(remote, exactly(handle, info.size, `upload ${local}`), info.size, { ...options, mode });
    } finally {
      await handle.close();
    }
  }

  /** copy a file out of a running sandbox */
  async download(remote: string, local: string): Promise<void> {
    const target = path.resolve(local);
    const temp = path.join(path.dirname(target), `.${path.basename(target)}.useshards-${randomBytes(8).toString("hex")}`);
    const handle = await fs.open(temp, "wx", 0o600);
    try {
      const info = await this.receive("files", remote, handle, false);
      await fs.chmod(temp, info.mode & 0o777);
      await fs.rename(temp, target);
    } catch (err) {
      await fs.rm(temp, { force: true });
      throw err;
    }
  }

  /** copy a directory into a running sandbox */
  async uploadDir(local: string, remote: string, options: { user?: string } = {}): Promise<void> {
    const { parent, name } = split(remote);
    if (!(await fs.stat(local)).isDirectory()) {
      throw notADirectory(`upload ${local}: not a directory`, local);
    }
    // The daemon streams the tar into the guest as it arrives, so it goes chunked; the guest's unpack may outlast any bound.
    await this.transport.fetch("PUT", this.route("archive"), {
      query: wire.query({ path: parent, user: options.user }),
      body: pack(local, name),
      timeoutMs: 0,
    });
  }

  /** copy a directory out of a running sandbox */
  async downloadDir(remote: string, local: string): Promise<void> {
    const { clean, name } = split(remote);
    const target = path.resolve(local);
    const spool = await fs.mkdtemp(path.join(os.tmpdir(), "useshards-"));
    try {
      // The whole tar is in before the unpack starts, so a cut never lands half a tree.
      const tar = path.join(spool, "archive.tar");
      const info = await this.receive("archive", clean, await fs.open(tar, "wx", 0o600), true);
      const made = await makeDir(target);
      await unpack(createReadStream(tar), target, name);
      // A directory already there keeps its mode, so only one this call made takes the sandbox's.
      if (made) {
        await fs.chmod(target, info.mode & 0o777);
      }
    } finally {
      await fs.rm(spool, { recursive: true, force: true });
    }
  }

  private async put(path: string, body: Uint8Array | AsyncIterable<Uint8Array>, length: number, options: WriteOptions): Promise<void> {
    const { mode, parents, user } = options;
    await this.transport.fetch("PUT", this.route("files"), {
      query: wire.query({ path, mode: mode?.toString(8), parents, user }),
      body,
      length,
    });
  }

  /** receive writes the body of a GET to handle, syncs and closes it, and answers the stat the daemon sent first. */
  private async receive(route: Route, remote: string, handle: fs.FileHandle, directory: boolean): Promise<FileInfo> {
    try {
      const { headers, body, cancel } = await this.transport.open("GET", this.route(route), { query: { path: remote } });
      const info = accepted(headers, remote, directory, cancel);
      // A write stream would hold a ref on the handle, and close() waits for every ref to go.
      await fs.writeFile(handle, body);
      await handle.sync();

      return info;
    } finally {
      await handle.close();
    }
  }

  private route(verb: Route): string {
    return wire.path("sandboxes", this.sandboxId, verb);
  }
}

function fileInfo(fields: Fields): FileInfo {
  return {
    type: fields.oneOf("type", fileTypes),
    size: fields.int("size"),
    mode: fields.int("mode"),
    uid: fields.int("uid"),
    gid: fields.int("gid"),
    mtime: fields.date("mtime"),
  };
}

function statOf(headers: IncomingHttpHeaders, what: string): FileInfo {
  const raw = headers[statHeader];
  if (typeof raw !== "string") {
    throw new ProtocolError(`${what}: the daemon answered no X-Shard-Stat header`);
  }
  let record: unknown;
  try {
    record = JSON.parse(raw);
  } catch {
    throw new ProtocolError(`${what}: the daemon answered an X-Shard-Stat header that is not JSON`);
  }

  return fileInfo(Fields.of(record, `the stat of ${what}`));
}

/** accepted reads the stat of a download, and lets go of the body unread when it refuses one. */
function accepted(headers: IncomingHttpHeaders, remote: string, directory: boolean, cancel: () => void): FileInfo {
  try {
    const info = statOf(headers, `download ${remote}`);
    if (directory && info.type !== "dir") {
      throw notADirectory(`download ${remote}: it is a ${info.type}, not a directory`, remote);
    }

    return info;
  } catch (err) {
    cancel();
    throw err;
  }
}

/** exactly yields size bytes of handle, and fails on a file that ended short or grew, so the daemon lands neither. */
async function* exactly(handle: fs.FileHandle, size: number, what: string): AsyncGenerator<Uint8Array> {
  const grew = (): UnknownLengthError => new UnknownLengthError(`${what}: the file grew past the ${size} bytes it held at the start`);
  for (let left = size; left > 0; ) {
    // The last read asks one byte more, so a file that grew fails before the daemon has every byte.
    const want = Math.min(chunkSize, left + 1);
    const { bytesRead, buffer } = await handle.read(Buffer.alloc(want), 0, want, null);
    if (bytesRead > left) {
      throw grew();
    }
    if (bytesRead === 0) {
      throw new UnknownLengthError(`${what}: the file ended ${left} bytes short of the ${size} it held at the start`);
    }
    left -= bytesRead;
    yield buffer.subarray(0, bytesRead);
  }
  const { bytesRead } = await handle.read(Buffer.alloc(1), 0, 1, null);
  if (bytesRead > 0) {
    throw grew();
  }
}

/** split answers the sandbox directory a tar lands in or comes from, and the name of its top entry. */
function split(remote: string): { clean: string; parent: string; name: string } {
  const clean = path.posix.normalize(remote).replace(/(.)\/+$/, "$1");
  const name = path.posix.basename(clean);
  if (!path.posix.isAbsolute(clean) || name === "" || name === "." || name === "..") {
    throw new TypeError(`${JSON.stringify(remote)} must be an absolute sandbox path below /`);
  }

  return { clean, parent: path.posix.dirname(clean), name };
}

/** makeDir makes target unless it is there, and answers whether it made it; a target that is not a directory is refused. */
async function makeDir(target: string): Promise<boolean> {
  const made = await fs.mkdir(target, 0o700).then(
    () => true,
    (err: unknown) => {
      if (isCode(err, "EEXIST")) {
        return false;
      }
      throw err;
    },
  );
  if (!(await fs.lstat(target)).isDirectory()) {
    throw notADirectory(`${target} is not a directory`, target);
  }

  return made;
}

function notADirectory(message: string, at: string): NodeJS.ErrnoException {
  const err: NodeJS.ErrnoException = new Error(`ENOTDIR: ${message}`);
  err.code = "ENOTDIR";
  err.path = at;

  return err;
}
