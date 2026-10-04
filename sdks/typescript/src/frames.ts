// RFC 6455 framing for the daemon's streams: the client masks every frame, the daemon masks none.
import { createHash, randomBytes } from "node:crypto";
import { ProtocolError } from "./errors.js";

export const opContinuation = 0x0;
export const opText = 0x1;
export const opBinary = 0x2;
export const opClose = 0x8;
export const opPing = 0x9;
export const opPong = 0xa;

export const closeNormal = 1000;

const acceptGuid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
const controlLimit = 125;
// The daemon sends a stream id and at most 1 MiB, so four times that is a broken peer, not a message.
export const messageLimit = 4 * 1024 * 1024;

export function handshakeKey(): string {
  return randomBytes(16).toString("base64");
}

export function acceptFor(key: string): string {
  return createHash("sha1").update(key + acceptGuid).digest("base64");
}

export function encodeFrame(opcode: number, payload: Uint8Array): Buffer {
  const length = payload.length;
  const extended = length < 126 ? 0 : length < 1 << 16 ? 2 : 8;
  const frame = Buffer.alloc(2 + extended + 4 + length);
  frame[0] = 0x80 | opcode;
  frame[1] = 0x80 | (extended === 0 ? length : extended === 2 ? 126 : 127);
  if (extended === 2) {
    frame.writeUInt16BE(length, 2);
  }
  if (extended === 8) {
    frame.writeBigUInt64BE(BigInt(length), 2);
  }
  const maskAt = 2 + extended;
  randomBytes(4).copy(frame, maskAt);
  for (let i = 0; i < length; i++) {
    frame[maskAt + 4 + i] = (payload[i] ?? 0) ^ (frame[maskAt + (i & 3)] ?? 0);
  }

  return frame;
}

export function encodeClose(code: number): Buffer {
  const payload = Buffer.alloc(2);
  payload.writeUInt16BE(code);

  return encodeFrame(opClose, payload);
}

export interface Message {
  opcode: number;
  payload: Buffer;
}

/** MessageReader turns the daemon's bytes into whole messages, with control frames passed through as they arrive. */
export class MessageReader {
  private buffer: Buffer = Buffer.alloc(0);
  private opcode: number | undefined;
  private fragments: Buffer[] = [];
  private fragmented = 0;

  feed(data: Buffer): Message[] {
    this.buffer = this.buffer.length === 0 ? data : Buffer.concat([this.buffer, data]);
    const messages: Message[] = [];
    for (let frame = this.nextFrame(); frame; frame = this.nextFrame()) {
      const message = this.assemble(frame.fin, frame.opcode, frame.payload);
      if (message) {
        messages.push(message);
      }
    }

    return messages;
  }

  private nextFrame(): { fin: boolean; opcode: number; payload: Buffer } | undefined {
    const buf = this.buffer;
    const first = buf[0];
    const second = buf[1];
    if (first === undefined || second === undefined) {
      return undefined;
    }
    if (first & 0x70) {
      throw new ProtocolError("the daemon sent a WebSocket frame with a reserved bit set");
    }
    if (second & 0x80) {
      throw new ProtocolError("the daemon sent a masked WebSocket frame, which only a client sends");
    }
    const fin = (first & 0x80) !== 0;
    const opcode = first & 0x0f;
    const short = second & 0x7f;
    let length = short;
    let offset = 2;
    if (short === 126) {
      if (buf.length < 4) {
        return undefined;
      }
      length = buf.readUInt16BE(2);
      offset = 4;
    }
    if (short === 127) {
      if (buf.length < 10) {
        return undefined;
      }
      const long = buf.readBigUInt64BE(2);
      length = long > BigInt(messageLimit) ? messageLimit + 1 : Number(long);
      offset = 10;
    }
    if (opcode >= opClose && (length > controlLimit || !fin)) {
      throw new ProtocolError("the daemon sent a WebSocket control frame that is fragmented or too long");
    }
    if (length > messageLimit) {
      throw new ProtocolError(`the daemon sent a WebSocket frame of more than ${messageLimit} bytes`);
    }
    if (buf.length < offset + length) {
      return undefined;
    }
    const payload = buf.subarray(offset, offset + length);
    this.buffer = buf.subarray(offset + length);

    return { fin, opcode, payload };
  }

  private assemble(fin: boolean, opcode: number, payload: Buffer): Message | undefined {
    if (![opContinuation, opText, opBinary, opClose, opPing, opPong].includes(opcode)) {
      throw new ProtocolError(`the daemon sent a WebSocket frame of unknown opcode ${opcode}`);
    }
    if (opcode >= opClose) {
      return { opcode, payload: Buffer.from(payload) };
    }
    if (opcode === opContinuation && this.opcode === undefined) {
      throw new ProtocolError("the daemon sent a continuation frame with no message to continue");
    }
    if (opcode !== opContinuation && this.opcode !== undefined) {
      throw new ProtocolError("the daemon started a WebSocket message inside another");
    }
    const kind = this.opcode ?? opcode;
    this.fragmented += payload.length;
    if (this.fragmented > messageLimit) {
      throw new ProtocolError(`the daemon sent a WebSocket message of more than ${messageLimit} bytes`);
    }
    this.fragments.push(payload);
    if (!fin) {
      this.opcode = kind;

      return undefined;
    }
    // concat copies, so a message never pins the read buffer it arrived in.
    const message = { opcode: kind, payload: Buffer.concat(this.fragments) };
    this.opcode = undefined;
    this.fragments = [];
    this.fragmented = 0;

    return message;
  }
}
