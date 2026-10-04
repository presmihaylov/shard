// The output a command result keeps: the newest bytes of stdout and stderr together, up to a limit.
import { stderr, stdout, type OutputStream } from "./wire.js";

export const defaultOutputLimit = 8 * 1024 * 1024;

interface Chunk {
  stream: OutputStream;
  bytes: Uint8Array;
}

export class OutputCapture {
  // A dropped chunk leaves an empty slot, so its bytes go at once and dropping from the front costs nothing.
  private chunks: Array<Chunk | undefined> = [];
  private head = 0;
  private size = 0;

  constructor(private readonly limit: number) {
    if (!Number.isSafeInteger(limit) || limit < 0) {
      throw new RangeError("outputLimitBytes must be a whole number of 0 or more");
    }
  }

  add(stream: OutputStream, bytes: Uint8Array): void {
    if (this.limit === 0 || bytes.length === 0) {
      return;
    }
    if (bytes.length >= this.limit) {
      this.chunks = [{ stream, bytes: bytes.subarray(bytes.length - this.limit) }];
      this.head = 0;
      this.size = this.limit;

      return;
    }
    this.chunks.push({ stream, bytes });
    this.size += bytes.length;
    while (this.size > this.limit) {
      const oldest = this.chunks[this.head];
      if (!oldest) {
        break;
      }
      const over = this.size - this.limit;
      if (oldest.bytes.length <= over) {
        this.chunks[this.head] = undefined;
        this.head++;
        this.size -= oldest.bytes.length;
        continue;
      }
      oldest.bytes = oldest.bytes.subarray(over);
      this.size -= over;
    }
    // The empty slots of a long command would otherwise grow the array without end.
    if (this.head > 1024 && this.head * 2 > this.chunks.length) {
      this.chunks = this.chunks.slice(this.head);
      this.head = 0;
    }
  }

  /** reset drops everything, as an attach replays the daemon's buffer from its oldest byte. */
  reset(): void {
    this.chunks = [];
    this.head = 0;
    this.size = 0;
  }

  output(): { stdout: Buffer; stderr: Buffer } {
    const held = this.chunks.slice(this.head).filter((chunk) => chunk !== undefined);

    return {
      stdout: Buffer.concat(held.filter((chunk) => chunk.stream === stdout).map((chunk) => chunk.bytes)),
      stderr: Buffer.concat(held.filter((chunk) => chunk.stream === stderr).map((chunk) => chunk.bytes)),
    };
  }
}
