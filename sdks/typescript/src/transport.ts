// The one connection pool every call rides, and every non-2xx answer thrown as an APIError.
import * as http from "node:http";
import * as https from "node:https";
import type { Duplex } from "node:stream";
import type { Settings } from "./config.js";
import { ConnectionError, ProtocolError, apiError } from "./errors.js";
import { version } from "./version.js";

export const defaultTimeoutMs = 60_000;

export interface CallOptions {
  json?: unknown;
  query?: Record<string, string>;
  signal?: AbortSignal | undefined;
  /** How long the daemon may take to answer; 0 is no bound, for a call that waits on the guest. */
  timeoutMs?: number;
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
  async call(method: string, path: string, options: CallOptions = {}): Promise<unknown> {
    const what = `${method} ${path}`;
    const body = options.json === undefined ? undefined : Buffer.from(JSON.stringify(options.json));
    const headers: http.OutgoingHttpHeaders = body ? { "Content-Type": "application/json", "Content-Length": body.length } : {};
    const req = this.request(method, path, headers, options);
    const answer = await new Promise<Buffer>((resolve, reject) => {
      req.on("response", (res) => {
        const status = res.statusCode ?? 0;
        read(res, what, options.signal).then(
          (bytes) => (status >= 200 && status < 300 ? resolve(bytes) : reject(apiError(status, bytes))),
          reject,
        );
      });
      req.on("error", (err) => reject(failed(what, err, options.signal)));
      req.end(body);
    });
    if (answer.length === 0) {
      return undefined;
    }
    try {
      return JSON.parse(answer.toString("utf8"));
    } catch {
      throw new ProtocolError(`${what} answered a body that is not JSON`);
    }
  }

  /** upgrade sends a WebSocket handshake; a refusal comes before the 101, as the status and the body any call gets. */
  upgrade(path: string, key: string, signal: AbortSignal | undefined): Promise<Upgraded> {
    const what = `GET ${path}`;
    const handshake = { Connection: "Upgrade", Upgrade: "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": key };
    const req = this.request("GET", path, handshake, { signal });

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
    const timeout = options.timeoutMs ?? this.timeoutMs;
    if (timeout > 0) {
      req.setTimeout(timeout, () => req.destroy(new ConnectionError(`${method} ${path}: the daemon did not answer in time`)));
    }

    return req;
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

function read(res: http.IncomingMessage, what: string, signal: AbortSignal | undefined): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    res.on("data", (chunk: Buffer) => chunks.push(chunk));
    res.on("end", () => resolve(Buffer.concat(chunks)));
    res.on("error", (err) => {
      reject(signal?.aborted ? signal.reason : new ConnectionError(`${what}: the answer was cut short: ${err.message}`, { cause: err }));
    });
    res.on("close", () => {
      if (!res.complete) {
        reject(signal?.aborted ? signal.reason : new ConnectionError(`${what}: the answer was cut short`));
      }
    });
  });
}
