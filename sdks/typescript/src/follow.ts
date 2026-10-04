// A log as it arrives, over one WebSocket that the end of the log, a fault, a break or an abort lets go of.
import { ConnectionError, ProtocolError, failureError } from "./errors.js";
import { closeNormal, opBinary, opText, type Message } from "./frames.js";
import { networkLogRecord, type NetworkLogRecord } from "./records.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";
import { WebSocket } from "./ws.js";

/** Parse reads one message, or answers undefined for the daemon's end of the log. */
type Parse<T> = (message: Message, what: string) => T | undefined;

export async function* follow<T>(transport: Transport, route: string, what: string, parse: Parse<T>, signal?: AbortSignal): AsyncGenerator<T> {
  const ws = await WebSocket.connect(transport, route, what, { query: { follow: "true" }, signal });
  // The goodbye wakes a receive in flight; the finally below waits for it.
  const abort = (): void => void ws.close();
  signal?.addEventListener("abort", abort, { once: true });
  try {
    for (;;) {
      const message = await ws.receive();
      signal?.throwIfAborted();
      if (message === undefined) {
        ended(ws, what);

        return;
      }
      const item = parse(message, what);
      if (item === undefined) {
        return;
      }
      yield item;
    }
  } finally {
    signal?.removeEventListener("abort", abort);
    const closing = ws.close();
    // An abort answers at once, as an exec's does; the goodbye finishes on its own.
    if (!signal?.aborted) {
      await closing;
    }
  }
}

/** ended throws unless the daemon closed the log normally, as on a stop or a remove. */
function ended(ws: WebSocket, what: string): void {
  const close = ws.peerClose;
  if (!close) {
    throw new ConnectionError(`${what}: the stream to the daemon dropped`);
  }
  if (close.code !== closeNormal) {
    throw failureError("internal", `${what}: ${close.reason || `closed with ${close.code}`}`);
  }
}

export function logChunk(message: Message, what: string): Uint8Array | undefined {
  const stream = message.payload[0];
  if (message.opcode === opBinary && stream === wire.stdout) {
    return message.payload.subarray(1);
  }
  if (message.opcode === opBinary && stream === wire.exit) {
    return undefined;
  }
  if (message.opcode === opBinary && stream === wire.failure) {
    throw wire.failureOf(message.payload.subarray(1), what);
  }
  throw new ProtocolError(`${what}: the daemon sent a message the SDK cannot read`);
}

export function networkLogEntry(message: Message, what: string): NetworkLogRecord {
  if (message.opcode !== opText) {
    throw new ProtocolError(`${what}: the daemon sent a binary message where a record belongs`);
  }
  let record: unknown;
  try {
    record = JSON.parse(message.payload.toString("utf8"));
  } catch {
    throw new ProtocolError(`${what}: the daemon sent a record that is not JSON`);
  }

  return networkLogRecord(record);
}
