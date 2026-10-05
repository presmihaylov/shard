// The one connection pool every call rides, and every non-2xx answer thrown as an APIError.
import * as http from "node:http";
import * as https from "node:https";
import { Readable, type Duplex, pipeline } from "node:stream";
import createClient, { type Client } from "openapi-fetch";
import type { Settings } from "./config.js";
import { ProtocolError, ShardConnectionError, apiError, statusError } from "./errors.js";
import type { paths } from "./generated/schema.js";
import { version } from "./version.js";

export const defaultTimeoutMs = 60_000;

const json = "application/json";

export interface CallOptions {
  query?: Record<string, string>;
  signal?: AbortSignal | undefined;
  /** How long the daemon may take to answer; 0 is no bound, for a call that waits on the guest. */
  timeoutMs?: number;
}

export interface SendOptions extends CallOptions {
  /** A raw body: bytes go with their length, an iterable goes chunked unless length names its size. */
  body?: Uint8Array | AsyncIterable<Uint8Array>;
  length?: number;
  /** The raw body's type; left out, application/octet-stream. */
  type?: string;
}

/** Answer is a 2xx: its status, its headers and its whole body. */
export interface Answer {
  status: number;
  headers: http.IncomingHttpHeaders;
  body: Buffer;
}

/** Api is the generated client of the public routes. Its types stay private to the SDK. */
export type Api = Client<paths>;

export type Fetch = (request: Request) => Promise<Response>;

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
  /** api sends every JSON route over this pool, bounded by the transport's timeout. */
  readonly api: Api;
  /** waiting is the fetch a call passes to api when it waits on the guest, so no bound cuts it. */
  readonly waiting: Fetch;
  private readonly base: URL;
  // ES private fields, so no print of any handle reaches the key, nor the pooled sockets that hold its header.
  readonly #agent: http.Agent;
  readonly #authorization: string;

  constructor(
    settings: Pick<Settings, "baseUrl" | "apiKey" | "ca">,
    private readonly timeoutMs = defaultTimeoutMs,
  ) {
    this.#authorization = `Bearer ${settings.apiKey}`;
    this.base = new URL(settings.baseUrl);
    this.#agent =
      this.base.protocol === "https:" ? new https.Agent({ keepAlive: true, ca: settings.ca }) : new http.Agent({ keepAlive: true });
    // Accept lets the fetcher refuse an answer that is not JSON before the generated client parses it.
    this.api = createClient<paths>({ baseUrl: this.base.origin, fetch: this.fetcher(this.timeoutMs), headers: { Accept: json } });
    this.waiting = this.fetcher(0);
  }

  /** fetcher is the fetch the generated client rides: this pool and key, and a refusal thrown as an APIError. */
  private fetcher(timeoutMs: number): Fetch {
    return async (request) => {
      const url = new URL(request.url);
      const path = url.pathname + url.search;
      const body = request.body ? new Uint8Array(await request.arrayBuffer()) : undefined;
      const type = request.headers.get("Content-Type") ?? undefined;
      const answer = await this.fetch(request.method, path, { body, type, signal: request.signal, timeoutMs });
      if (answer.body.length > 0 && request.headers.get("Accept") === json) {
        try {
          JSON.parse(answer.body.toString("utf8"));
        } catch {
          throw new ProtocolError(`${request.method} ${url.pathname} answered a body that is not JSON`);
        }
      }
      const empty = answer.status === 204 || answer.status === 205 || answer.status === 304;

      return new Response(empty ? null : answer.body, { status: answer.status, headers: headersOf(answer.headers) });
    };
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

    return { status, headers: res.headers, body };
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
    this.#agent.destroy();
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
      agent: this.#agent,
      signal: options.signal,
      headers: { ...headers, Authorization: this.#authorization, "User-Agent": `useshards-typescript/${version}` },
    };
    const req = this.base.protocol === "https:" ? https.request(url, settings) : http.request(url, settings);
    // Set even at 0, so a pooled socket keeps no bound an earlier call left on it.
    req.setTimeout(options.timeoutMs ?? this.timeoutMs, () => {
      req.destroy(new ShardConnectionError(`${method} ${path}: the daemon did not answer in time`));
    });

    return req;
  }
}

function encode(options: SendOptions): { headers: http.OutgoingHttpHeaders; body: Uint8Array | AsyncIterable<Uint8Array> | undefined } {
  if (options.body === undefined) {
    return { headers: {}, body: undefined };
  }
  const length = options.body instanceof Uint8Array ? options.body.length : options.length;
  const headers: http.OutgoingHttpHeaders = { "Content-Type": options.type ?? "application/octet-stream" };
  if (length !== undefined) {
    headers["Content-Length"] = length;
  }

  return { headers, body: options.body };
}

function headersOf(incoming: http.IncomingHttpHeaders): Headers {
  const headers = new Headers();
  for (const [key, value] of Object.entries(incoming)) {
    for (const item of Array.isArray(value) ? value : [value ?? ""]) {
      headers.append(key, item);
    }
  }

  return headers;
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
  if (err instanceof ShardConnectionError) {
    return err;
  }

  return new ShardConnectionError(`${what}: ${err.message || err.name}`, { cause: err });
}

function cut(what: string, err: unknown, signal: AbortSignal | undefined): unknown {
  if (signal?.aborted) {
    return signal.reason;
  }
  if (err instanceof ShardConnectionError) {
    return err;
  }
  const detail = err instanceof Error ? `: ${err.message}` : "";

  return new ShardConnectionError(`${what}: the answer was cut short${detail}`, { cause: err });
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
