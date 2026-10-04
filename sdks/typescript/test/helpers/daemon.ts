// A fake daemon over plain HTTP, with the server half of the WebSocket the SDK's src/ never needs.
import { once } from "node:events";
import * as http from "node:http";
import * as https from "node:https";
import type { Duplex } from "node:stream";
import { acceptFor, opBinary, opClose, opText } from "../../src/frames.js";
import type { Certificate } from "./tls.js";

export interface Frame {
  opcode: number;
  payload: Buffer;
}

/** serverFrame is one unmasked frame, as the daemon writes it. */
export function serverFrame(opcode: number, payload: Uint8Array, fin = true): Buffer {
  const length = payload.length;
  const extended = length < 126 ? 0 : length < 1 << 16 ? 2 : 8;
  const header = Buffer.alloc(2 + extended);
  header[0] = (fin ? 0x80 : 0) | opcode;
  header[1] = extended === 0 ? length : extended === 2 ? 126 : 127;
  if (extended === 2) {
    header.writeUInt16BE(length, 2);
  }
  if (extended === 8) {
    header.writeBigUInt64BE(BigInt(length), 2);
  }

  return Buffer.concat([header, payload]);
}

/** ClientFrames reads the masked frames a client writes, from bytes split anywhere. */
export class ClientFrames {
  private buffer = Buffer.alloc(0);

  feed(data: Buffer): Frame[] {
    this.buffer = Buffer.concat([this.buffer, data]);
    const frames: Frame[] = [];
    for (;;) {
      const frame = this.next();
      if (!frame) {
        return frames;
      }
      frames.push(frame);
    }
  }

  private next(): Frame | undefined {
    const buf = this.buffer;
    if (buf.length < 2) {
      return undefined;
    }
    const second = buf[1] ?? 0;
    if (!(second & 0x80)) {
      throw new Error("the client sent an unmasked frame");
    }
    let length = second & 0x7f;
    let offset = 2;
    if (length === 126) {
      if (buf.length < 4) {
        return undefined;
      }
      length = buf.readUInt16BE(2);
      offset = 4;
    }
    if (length === 127) {
      if (buf.length < 10) {
        return undefined;
      }
      length = Number(buf.readBigUInt64BE(2));
      offset = 10;
    }
    if (buf.length < offset + 4 + length) {
      return undefined;
    }
    const mask = buf.subarray(offset, offset + 4);
    const payload = Buffer.alloc(length);
    for (let i = 0; i < length; i++) {
      payload[i] = (buf[offset + 4 + i] ?? 0) ^ (mask[i & 3] ?? 0);
    }
    this.buffer = buf.subarray(offset + 4 + length);

    return { opcode: (buf[0] ?? 0) & 0x0f, payload };
  }
}

export function decodeClientFrames(bytes: Buffer): Frame[] {
  return new ClientFrames().feed(bytes);
}

/** Peer is the daemon's end of one attach. */
export class Peer {
  private readonly frames = new ClientFrames();
  private readonly inbox: Frame[] = [];
  private waiter: (() => void) | undefined;
  private closeSent = false;
  readonly ended: Promise<void>;

  constructor(
    readonly socket: Duplex,
    readonly path: string,
  ) {
    socket.on("data", (data: Buffer) => {
      const frames = this.frames.feed(data);
      // A daemon answers the client's goodbye with its own, as RFC 6455 asks.
      if (frames.some((frame) => frame.opcode === opClose)) {
        this.close();
      }
      this.inbox.push(...frames);
      this.waiter?.();
    });
    socket.on("error", () => undefined);
    // http.Server sockets are half-open, and the daemon drops the connection once the client ends its side.
    socket.on("end", () => socket.end());
    this.ended = once(socket, "close").then(() => undefined);
  }

  send(stream: number, data: Uint8Array | string): void {
    this.socket.write(serverFrame(opBinary, Buffer.concat([Buffer.of(stream), Buffer.from(data)])));
  }

  exit(fields: Record<string, unknown>): void {
    this.send(3, JSON.stringify(fields));
  }

  close(code = 1000, reason = ""): void {
    if (this.closeSent || !this.socket.writable) {
      return;
    }
    this.closeSent = true;
    const payload = Buffer.alloc(2);
    payload.writeUInt16BE(code);
    this.socket.write(serverFrame(opClose, Buffer.concat([payload, Buffer.from(reason)])));
  }

  text(data: string): void {
    this.socket.write(serverFrame(opText, Buffer.from(data)));
  }

  /** next answers the client's next frame, or undefined once its connection is gone. */
  async next(): Promise<Frame | undefined> {
    for (;;) {
      const frame = this.inbox.shift();
      if (frame) {
        return frame;
      }
      if (this.socket.destroyed) {
        return undefined;
      }
      await Promise.race([new Promise<void>((resolve) => (this.waiter = resolve)), this.ended]);
    }
  }
}

export interface Request {
  method: string;
  url: URL;
  headers: http.IncomingHttpHeaders;
  body: string;
  bytes: Buffer;
}

export interface Answer {
  status: number;
  json?: unknown;
  raw?: string | Uint8Array;
  headers?: Record<string, string>;
}

type Route = (request: Request) => Answer | Promise<Answer> | undefined;
type Upgrade = (peer: Peer, request: Request) => void | Answer;

export class FakeDaemon {
  readonly requests: Request[] = [];
  readonly peers: Peer[] = [];
  route: Route = () => undefined;
  upgrade: Upgrade = () => ({ status: 404, json: { error: { code: "not_found", message: "no such command" } } });
  private readonly server: http.Server;
  private waiting: Array<() => void> = [];

  private constructor(tls: Certificate | undefined) {
    this.server = tls ? https.createServer({ cert: tls.cert, key: tls.key }) : http.createServer();
    this.server.on("request", (req: http.IncomingMessage, res: http.ServerResponse) => {
      this.collect(req).then(
        async (request) => {
          const answer = (await this.route(request)) ?? { status: 404, json: { error: { code: "not_found", message: "no route" } } };
          write(res, answer);
        },
        () => res.destroy(),
      );
    });
    this.server.on("upgrade", (req: http.IncomingMessage, socket: Duplex) => {
      const request = { method: req.method ?? "", url: new URL(req.url ?? "/", "http://daemon"), headers: req.headers, body: "", bytes: Buffer.alloc(0) };
      this.requests.push(request);
      const peer = new Peer(socket, request.url.pathname);
      const refusal = this.upgrade(peer, request);
      if (refusal) {
        const body = JSON.stringify(refusal.json ?? {});
        socket.end(`HTTP/1.1 ${refusal.status} Refused\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\nConnection: close\r\n\r\n${body}`);

        return;
      }
      this.peers.push(peer);
      const key = String(req.headers["sec-websocket-key"] ?? "");
      socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${acceptFor(key)}\r\n\r\n`);
      for (const wake of this.waiting.splice(0)) {
        wake();
      }
    });
  }

  static async start(tls?: Certificate): Promise<FakeDaemon> {
    const daemon = new FakeDaemon(tls);
    daemon.server.listen(0, "127.0.0.1");
    await once(daemon.server, "listening");

    return daemon;
  }

  get url(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  get port(): number {
    const address = this.server.address();
    if (!address || typeof address === "string") {
      throw new Error("the fake daemon listens on no port");
    }

    return address.port;
  }

  /** peer answers the n-th attach, once it arrives. */
  async peer(index: number): Promise<Peer> {
    for (;;) {
      const peer = this.peers[index];
      if (peer) {
        return peer;
      }
      await new Promise<void>((resolve) => this.waiting.push(resolve));
    }
  }

  async close(): Promise<void> {
    this.server.closeAllConnections();
    for (const peer of this.peers) {
      peer.socket.destroy();
    }
    this.server.close();
    await once(this.server, "close");
  }

  private async collect(req: http.IncomingMessage): Promise<Request> {
    const chunks: Buffer[] = [];
    for await (const chunk of req) {
      chunks.push(Buffer.from(chunk));
    }
    const bytes = Buffer.concat(chunks);
    const request = {
      method: req.method ?? "",
      url: new URL(req.url ?? "/", "http://daemon"),
      headers: req.headers,
      body: bytes.toString(),
      bytes,
    };
    this.requests.push(request);

    return request;
  }
}

function write(res: http.ServerResponse, answer: Answer): void {
  const headers = answer.headers ?? {};
  if (answer.raw !== undefined) {
    res.writeHead(answer.status, headers);
    res.end(answer.raw);

    return;
  }
  if (answer.json === undefined) {
    res.writeHead(answer.status, headers);
    res.end();

    return;
  }
  res.writeHead(answer.status, { "Content-Type": "application/json", ...headers });
  res.end(JSON.stringify(answer.json));
}
