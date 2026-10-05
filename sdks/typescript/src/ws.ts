// One WebSocket to the daemon, on a connection the transport opened and upgraded.
import type { Duplex } from "node:stream";
import { ProtocolError, ShardConnectionError } from "./errors.js";
import {
  MessageReader,
  acceptFor,
  closeNormal,
  encodeClose,
  encodeFrame,
  handshakeKey,
  opBinary,
  opClose,
  opPing,
  opPong,
  type Message,
} from "./frames.js";
import type { CallOptions, Transport } from "./transport.js";

// RFC 6455 reserves this code for a close frame that carried none.
const closeNoStatus = 1005;
// How long a goodbye waits for the daemon's own close before the connection is let go.
const closeWaitMs = 5_000;
// A reader that falls this many messages behind stops the socket, so a slow consumer stalls the daemon, not memory.
const highWater = 16;
// The write errors that only say the stream was gone, which the command's exit may explain.
const streamEnds = new WeakSet<Error>();

/** endedStream says a write failed because its stream was gone, not for a fault of the write's own. */
export function endedStream(err: unknown): boolean {
  return err instanceof Error && streamEnds.has(err);
}

/** streamEnd is the error of a write that found its stream gone. */
export function streamEnd(message: string, options?: ErrorOptions): ShardConnectionError {
  const err = new ShardConnectionError(message, options);
  streamEnds.add(err);

  return err;
}

export class WebSocket {
  private readonly reader = new MessageReader();
  private inbox: Message[] = [];
  // receive and a closing goodbye may wait at once, so every arrival wakes them all.
  private waiters: Array<() => void> = [];
  private failure: Error | undefined;
  private peerClosed = false;
  private closeFrame: Buffer | undefined;
  private dropped = false;
  private closeSent = false;
  // An ES private field: an upgraded TLS socket keeps its connect options, the Authorization header among them.
  readonly #socket: Duplex;

  private constructor(
    socket: Duplex,
    private readonly what: string,
  ) {
    this.#socket = socket;
    socket.on("data", (data: Buffer) => this.received(data));
    socket.on("end", () => this.drop(undefined));
    socket.on("close", () => this.drop(undefined));
    socket.on("error", (err) => this.drop(new ShardConnectionError(`${what}: the stream to the daemon dropped`, { cause: err })));
  }

  static async connect(transport: Transport, path: string, what: string, options: Pick<CallOptions, "query" | "signal"> = {}): Promise<WebSocket> {
    const key = handshakeKey();
    const { socket, head, headers } = await transport.upgrade(path, key, options);
    if (headers["sec-websocket-accept"] !== acceptFor(key)) {
      socket.destroy();
      throw new ProtocolError(`${what}: the daemon answered the WebSocket handshake with a key that does not match`);
    }
    const ws = new WebSocket(socket, what);
    if (head.length > 0) {
      ws.received(head);
    }

    return ws;
  }

  /** peerClose is the code and the reason of the daemon's close, or undefined while none came; a bare close is 1005. */
  get peerClose(): { code: number; reason: string } | undefined {
    const frame = this.closeFrame;
    if (!frame) {
      return undefined;
    }
    if (frame.length < 2) {
      return { code: closeNoStatus, reason: "" };
    }

    return { code: frame.readUInt16BE(0), reason: frame.subarray(2).toString("utf8") };
  }

  /** ended says nothing more arrives: the daemon's close came, or the connection ended. */
  private get ended(): boolean {
    return this.peerClosed || this.dropped;
  }

  sendBinary(payload: Uint8Array): Promise<void> {
    if (this.closeSent || this.ended) {
      return Promise.reject(streamEnd(`${this.what}: the stream to the daemon is closed`));
    }

    return this.write(encodeFrame(opBinary, payload));
  }

  /** receive answers the next text or binary message, or undefined once the stream ended or this client said goodbye. */
  async receive(): Promise<Message | undefined> {
    for (;;) {
      if (this.closeSent) {
        return undefined;
      }
      const message = this.take();
      if (message && message.opcode === opClose) {
        this.peerClosed = true;
        this.closeFrame = message.payload;

        return undefined;
      }
      if (message) {
        return message;
      }
      if (this.failure) {
        throw this.failure;
      }
      if (this.ended) {
        return undefined;
      }
      await new Promise<void>((resolve) => this.waiters.push(resolve));
    }
  }

  /** take removes the next message from the inbox, and lets the socket read again once the inbox is below the high water mark. */
  private take(): Message | undefined {
    const message = this.inbox.shift();
    if (message && this.inbox.length < highWater) {
      this.#socket.resume();
    }

    return message;
  }

  /** close says goodbye, or answers the daemon's, waits a moment for its close, then lets go; a peer already gone is all a close asks for. */
  async close(): Promise<void> {
    if (!this.closeSent && !this.dropped) {
      this.closeSent = true;
      this.notify();
      await this.write(encodeClose(closeNormal)).catch((err: unknown) => {
        this.failure ??= err instanceof Error ? err : new Error(String(err));
      });
    }
    const deadline = Date.now() + closeWaitMs;
    while (!this.ended && !this.failure && Date.now() < deadline) {
      this.inbox = this.inbox.filter((message) => message.opcode === opClose);
      if (this.inbox.length > 0) {
        this.peerClosed = true;
        continue;
      }
      this.#socket.resume();
      await this.waitFor(deadline - Date.now());
    }
    this.#socket.destroy();
  }

  private received(data: Buffer): void {
    let messages: Message[];
    try {
      messages = this.reader.feed(data);
    } catch (err) {
      this.drop(err instanceof Error ? err : new ProtocolError(String(err)));
      this.#socket.destroy();

      return;
    }
    for (const message of messages) {
      if (message.opcode === opPing && !this.closeSent) {
        this.write(encodeFrame(opPong, message.payload)).catch((err: unknown) => this.drop(err instanceof Error ? err : undefined));
        continue;
      }
      if (message.opcode === opPing || message.opcode === opPong) {
        continue;
      }
      this.inbox.push(message);
    }
    if (this.inbox.length >= highWater) {
      this.#socket.pause();
    }
    this.notify();
  }

  private drop(failure: Error | undefined): void {
    this.dropped = true;
    this.failure ??= failure;
    this.notify();
  }

  private notify(): void {
    for (const wake of this.waiters.splice(0)) {
      wake();
    }
  }

  private waitFor(ms: number): Promise<void> {
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((waiter) => waiter !== wake);
        resolve();
      }, ms);
      const wake = (): void => {
        clearTimeout(timer);
        resolve();
      };
      this.waiters.push(wake);
    });
  }

  private write(frame: Buffer): Promise<void> {
    return new Promise((resolve, reject) => {
      this.#socket.write(frame, (err) => {
        if (err) {
          reject(streamEnd(`${this.what}: the stream to the daemon dropped`, { cause: err }));

          return;
        }
        resolve();
      });
    });
  }
}
