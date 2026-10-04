// The one connection pool every call rides, and every non-2xx answer thrown as an APIError.
import * as http from "node:http";
import * as https from "node:https";
import { Readable, type Duplex, pipeline } from "node:stream";
import type { Settings } from "./config.js";
import { ConnectionError, ProtocolError, apiError, statusError } from "./errors.js";
import { version } from "./version.js";

export const defaultTimeoutMs = 60_000;

export interface CallOptions {
  json?: unknown;
  query?: Record<string, string>;
  signal?: AbortSignal | undefined;
  /** How long the daemon may take to answer; 0 is no bound, for a call that waits on the guest. */
  timeoutMs?: number;
}

export interface SendOptions extends CallOptions {
  /** A raw body: bytes go with their length, an iterable goes chunked unless length names its size. */
  body?: Uint8Array | AsyncIterable<Uint8Array>;
  length?: number;
}

/** Answer is a 2xx: its headers and its whole body. */
export interface Answer {
  headers: http.IncomingHttpHeaders;
  body: Buffer;
}

/** Opened is a 2xx whose body the caller reads as it arrives, or cancels to let go of it unread. */
export interface Opened {
  headers: http.IncomingHttpHeaders;
  body: AsyncGenerator<Buffer>;
  cancel(): void;
}

export interface Upgraded {
  socket: Duplex;
  head: Buffer;
  headers: http.IncomingHttpHeaders;
}

export class Transport {
  private readonly agent: http.Agent;
  private readonly base: URL;

  // An http base is for the unit tests' fake daemon; resolve() lets nothing but https through.
  constructor(
    private readonly settings: Pick<Settings, "baseUrl" | "apiKey" | "ca">,
    private readonly timeoutMs = defaultTimeoutMs,
  ) {
    this.base = new URL(settings.baseUrl);
    this.agent =
      this.base.protocol === "https:" ? new https.Agent({ keepAlive: true, ca: settings.ca }) : new http.Agent({ keepAlive: true });
  }

  /** call sends one request and answers its JSON body, or undefined for an empty one. */
  async call(method: string, path: string, options: SendOptions = {}): Promise<unknown> {
    const { body } = await this.fetch(method, path, options);
    if (body.length === 0) {
      return undefined;
    }
    try {
      return JSON.parse(body.toString("utf8"));
    } catch {
      throw new ProtocolError(`${method} ${path} answered a body that is not JSON`);
    }
  }

  /** fetch answers a 2xx with its whole body. */
  async fetch(method: string, path: string, options: SendOptions = {}): Promise<Answer> {
    const what = `${method} ${path}`;
    const res = await this.send(method, path, options);
    const body = await read(res, what, options.signal);
    const status = res.statusCode ?? 0;
    if (!ok(status)) {
      throw apiError(status, body);
    }

    return { headers: res.headers, body };
  }

  /** open answers a 2xx once its headers arrive; a refusal is read whole and thrown. */
  async open(method: string, path: string, options: SendOptions = {}): Promise<Opened> {
    const what = `${method} ${path}`;
    const res = await this.send(method, path, options);
    const status = res.statusCode ?? 0;
    if (!ok(status)) {
      throw apiError(status, await read(res, what, options.signal));
    }

    return { headers: res.headers, body: stream(res, what, options.signal), cancel: () => res.destroy() };
  }

  /** head answers a 2xx's headers; a refusal to a HEAD has no body, so what names the request in its error. */
  async head(path: string, what: string, options: CallOptions = {}): Promise<http.IncomingHttpHeaders> {
    const res = await this.send("HEAD", path, options);
    await read(res, `HEAD ${path}`, options.signal);
    const status = res.statusCode ?? 0;
    if (!ok(status)) {
      throw statusError(status, `the daemon answered ${status} to ${what}`);
    }

    return res.headers;
  }

  /** upgrade sends a WebSocket handshake; a refusal comes before the 101, as the status and the body any call gets. */
  upgrade(path: string, key: string, options: Pick<CallOptions, "query" | "signal">): Promise<Upgraded> {
    const what = `GET ${path}`;
    const { signal } = options;
    const handshake = { Connection: "Upgrade", Upgrade: "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": key };
    const req = this.request("GET", path, handshake, options);

    return new Promise((resolve, reject) => {
      req.on("upgrade", (res, socket, head) => {
        // The bound is on the handshake; an attach may sit quiet for as long as its command does.
        socket.setTimeout(0);
        resolve({ socket, head, headers: res.headers });
      });
      req.on("response", (res) => {
        read(res, what, signal).then((bytes) => reject(apiError(res.statusCode ?? 0, bytes)), reject);
      });
      req.on("error", (err) => reject(failed(what, err, signal)));
      req.end();
    });
  }

  close(): void {
    this.agent.destroy();
  }

  /** send resolves on the response, whatever its status; a body source that fails rejects with its own error. */
  private send(method: string, path: string, options: SendOptions): Promise<http.IncomingMessage> {
    const what = `${method} ${path}`;
    const { headers, body } = encode(options);
    const req = this.request(method, path, headers, options);
    let sourceError: unknown;

    return new Promise((resolve, reject) => {
      const fail = (err: Error): void => reject(sourceError ?? failed(what, err, options.signal));
      req.on("response", resolve);
      req.on("error", fail);
      if (body === undefined || body instanceof Uint8Array) {
        req.end(body);

        return;
      }
      const source = guarded(body, (err) => {
        sourceError = err;
      });
      pipeline(Readable.from(source, { objectMode: false }), req, (err) => {
        if (err) {
          fail(err);
        }
      });
    });
  }

  private request(method: string, path: string, headers: http.OutgoingHttpHeaders, options: CallOptions): http.ClientRequest {
    const url = new URL(path, this.base);
    for (const [key, value] of Object.entries(options.query ?? {})) {
      url.searchParams.set(key, value);
    }
    const settings: http.RequestOptions = {
      method,
      agent: this.agent,
      signal: options.signal,
      headers: { ...headers, Authorization: `Bearer ${this.settings.apiKey}`, "User-Agent": `useshards-typescript/${version}` },
    };
    const req = this.base.protocol === "https:" ? https.request(url, settings) : http.request(url, settings);
    // Set even at 0, so a pooled socket keeps no bound an earlier call left on it.
    req.setTimeout(options.timeoutMs ?? this.timeoutMs, () => {
      req.destroy(new ConnectionError(`${method} ${path}: the daemon did not answer in time`));
    });

    return req;
  }
}

function encode(options: SendOptions): { headers: http.OutgoingHttpHeaders; body: Uint8Array | AsyncIterable<Uint8Array> | undefined } {
  if (options.json !== undefined) {
    const body = Buffer.from(JSON.stringify(options.json));

    return { headers: { "Content-Type": "application/json", "Content-Length": body.length }, body };
  }
  if (options.body === undefined) {
    return { headers: {}, body: undefined };
  }
  const length = options.body instanceof Uint8Array ? options.body.length : options.length;
  const headers: http.OutgoingHttpHeaders = { "Content-Type": "application/octet-stream" };
  if (length !== undefined) {
    headers["Content-Length"] = length;
  }

  return { headers, body: options.body };
}

function ok(status: number): boolean {
  return status >= 200 && status < 300;
}

/** guarded records the error its source throws, since the request it streams into fails with a plainer one. */
async function* guarded(source: AsyncIterable<Uint8Array>, record: (err: unknown) => void): AsyncGenerator<Uint8Array> {
  try {
    yield* source;
  } catch (err) {
    record(err);
    throw err;
  }
}

/** failed names a request that got no answer; an abort is the caller's own reason, as fetch throws it. */
function failed(what: string, err: Error, signal: AbortSignal | undefined): unknown {
  if (signal?.aborted) {
    return signal.reason;
  }
  if (err instanceof ConnectionError) {
    return err;
  }

  return new ConnectionError(`${what}: ${err.message || err.name}`, { cause: err });
}

function cut(what: string, err: unknown, signal: AbortSignal | undefined): unknown {
  if (signal?.aborted) {
    return signal.reason;
  }
  if (err instanceof ConnectionError) {
    return err;
  }
  const detail = err instanceof Error ? `: ${err.message}` : "";

  return new ConnectionError(`${what}: the answer was cut short${detail}`, { cause: err });
}

/** stream yields a body as it arrives, and fails on one that ends before its last byte. */
async function* stream(res: http.IncomingMessage, what: string, signal: AbortSignal | undefined): AsyncGenerator<Buffer> {
  try {
    for await (const chunk of res) {
      yield Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk));
    }
  } catch (err) {
    throw cut(what, err, signal);
  }
  if (!res.complete) {
    throw cut(what, undefined, signal);
  }
}

function read(res: http.IncomingMessage, what: string, signal: AbortSignal | undefined): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    res.on("data", (chunk: Buffer) => chunks.push(chunk));
    res.on("end", () => resolve(Buffer.concat(chunks)));
    res.on("error", (err) => reject(cut(what, err, signal)));
    res.on("close", () => {
      if (!res.complete) {
        reject(cut(what, undefined, signal));
      }
    });
  });
}
