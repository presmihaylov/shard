// One command in a sandbox over the daemon's stream: its start, its attaches, and how it ended.
import type { OutputCapture } from "./capture.js";
import { date, isStrings } from "./decode.js";
import { ProtocolError, ShardConnectionError, isObject } from "./errors.js";
import { opBinary } from "./frames.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";
import { WebSocket, streamEnd } from "./ws.js";

export interface Handlers {
  onStdout?: ((chunk: Uint8Array) => void) | undefined;
  onStderr?: ((chunk: Uint8Array) => void) | undefined;
}

export type CommandState = "running" | "exited";

/** CommandInfo is a command as the daemon holds it, which any client can read. */
export interface CommandInfo {
  id: string;
  sandboxId: string;
  command: string[];
  state: CommandState;
  /** The exit code once the command ended, or null while it runs; a signal ends it with 128 plus its number, as 143 for TERM. */
  exitCode: number | null;
  /** The signal the server reported, or null; every provider reports a signal in the exit code instead. */
  signal: number | null;
  startedAt: Date;
  exitedAt: Date | null;
  /** Output the daemon dropped before any client read it. */
  lostBytes: number;
}

export class Session {
  private ws: WebSocket | undefined;
  // undefined is an attach this client let go of before the command ended.
  private reading: Promise<wire.Exit | undefined> | undefined;
  private closing: Promise<void> = Promise.resolve();
  private writing: Promise<void> = Promise.resolve();
  private exited: wire.Exit | undefined;
  private readonly what: string;

  constructor(
    private readonly transport: Transport,
    readonly sandboxId: string,
    readonly id: string,
    private readonly capture: OutputCapture,
    private readonly handlers: Handlers,
  ) {
    this.what = `the command ${id} in sandbox ${sandboxId}`;
  }

  /** start runs the command and attaches at once; an abort lets go of the stream and leaves the command running. */
  static async start(
    transport: Transport,
    sandboxId: string,
    request: wire.ExecRequest,
    capture: OutputCapture,
    handlers: Handlers,
    signal?: AbortSignal,
  ): Promise<Session> {
    const { data } = await transport.api.POST("/v0/sandboxes/{id}/exec", { params: { path: { id: sandboxId } }, body: request, signal });
    const record = commandInfo(data);
    const session = new Session(transport, sandboxId, record.id, capture, handlers);
    await session.attach(signal);

    return session;
  }

  /** attach opens a stream. Every attach replays the daemon's buffer from its oldest byte, so the capture starts again. */
  async attach(signal?: AbortSignal): Promise<void> {
    this.disconnect();
    await this.closing;
    const ws = await WebSocket.connect(this.transport, this.path(), this.what, { signal });
    this.capture.reset();
    this.ws = ws;
    const reading = this.read(ws);
    // wait() hands this rejection to the caller; the handler only keeps a command nobody waits on from ending the process.
    reading.then(undefined, () => undefined);
    this.reading = reading;
    if (!signal) {
      return;
    }
    const abort = (): void => {
      if (this.ws === ws) {
        this.disconnect();
      }
    };
    signal.addEventListener("abort", abort, { once: true });
    reading.finally(() => signal.removeEventListener("abort", abort)).then(undefined, () => undefined);
  }

  /** disconnect lets go of the stream; the command runs on, and a later attach replays what the daemon still holds. */
  disconnect(): void {
    const ws = this.ws;
    if (!ws) {
      return;
    }
    this.ws = undefined;
    this.closing = ws.close();
  }

  /** wait answers how the command ended. With no stream it attaches, and the daemon replays its buffer, then the exit. */
  async wait(signal?: AbortSignal): Promise<wire.Exit> {
    signal?.throwIfAborted();
    if (this.exited) {
      return this.exited;
    }
    let exit = await this.until(this.reading, signal);
    signal?.throwIfAborted();
    if (!exit) {
      await this.attach(signal);
      exit = await this.until(this.reading, signal);
      signal?.throwIfAborted();
    }
    if (!exit) {
      throw new ShardConnectionError(`${this.what}: the stream was let go before the command ended`);
    }
    this.exited = exit;

    return exit;
  }

  /** until answers how a stream ended, and lets go of it if the caller's signal aborts first, whichever signal opened it. */
  private async until(reading: Promise<wire.Exit | undefined> | undefined, signal?: AbortSignal): Promise<wire.Exit | undefined> {
    if (!reading || !signal) {
      return reading;
    }
    const abort = (): void => {
      if (this.reading === reading) {
        this.disconnect();
      }
    };
    signal.addEventListener("abort", abort, { once: true });
    try {
      return await reading;
    } finally {
      signal.removeEventListener("abort", abort);
    }
  }

  /** endedWithExit answers, once the open stream ends, whether it carried the command's exit; wait() reports any other end. */
  async endedWithExit(): Promise<boolean> {
    if (this.exited) {
      return true;
    }
    const exit = await this.reading?.then(undefined, () => undefined);

    return exit !== undefined;
  }

  async inspect(): Promise<CommandInfo> {
    return commandInfo((await this.transport.api.GET("/v0/sandboxes/{id}/exec/{exec}", { params: this.params })).data);
  }

  async kill(signal = ""): Promise<void> {
    await this.transport.api.POST("/v0/sandboxes/{id}/exec/{exec}/kill", { params: this.params, body: signal ? { signal } : {} });
  }

  async resize(size: wire.TerminalSize): Promise<void> {
    await this.transport.api.POST("/v0/sandboxes/{id}/exec/{exec}/resize", { params: this.params, body: size });
  }

  /** writeStdin sends data in order, split into the most the daemon reads in one message. */
  writeStdin(data: string | Uint8Array): Promise<void> {
    const bytes = typeof data === "string" ? Buffer.from(data) : data;

    return this.send(async (ws) => {
      for (let at = 0; at < bytes.length; at += wire.maxPayload) {
        await ws.sendBinary(message(wire.stdin, bytes.subarray(at, at + wire.maxPayload)));
      }
    });
  }

  /** closeStdin tells the command its input has ended, as a guest that reads waits for that. */
  closeStdin(): Promise<void> {
    return this.send((ws) => ws.sendBinary(message(wire.stdinClose, new Uint8Array(0))));
  }

  /** send queues behind earlier writes, so two calls never interleave their chunks. */
  private send(write: (ws: WebSocket) => Promise<void>): Promise<void> {
    const ws = this.ws;
    if (!ws) {
      return Promise.reject(streamEnd(`${this.what} is not attached`));
    }
    const sent = this.writing.then(() => write(ws));
    this.writing = sent.then(undefined, () => undefined);

    return sent;
  }

  private async read(ws: WebSocket): Promise<wire.Exit | undefined> {
    let exit: wire.Exit | undefined;
    let failed: unknown;
    try {
      exit = await this.pump(ws);
    } catch (err) {
      failed = err;
    }
    const released = this.ws !== ws;
    if (!released) {
      this.ws = undefined;
      this.closing = ws.close();
    }
    if (exit) {
      return exit;
    }
    // A stream this client let go of ends in whatever state the release left it.
    if (released) {
      return undefined;
    }
    if (failed !== undefined && !(failed instanceof ShardConnectionError)) {
      throw failed;
    }
    throw await this.cut(failed);
  }

  /** pump hands the output on until the exit, and answers undefined if the stream ends first. */
  private async pump(ws: WebSocket): Promise<wire.Exit | undefined> {
    for (let message = await ws.receive(); message; message = await ws.receive()) {
      const stream = message.payload[0];
      if (message.opcode !== opBinary || stream === undefined) {
        throw new ProtocolError(`${this.what}: the daemon sent a message that names no stream`);
      }
      const body = message.payload.subarray(1);
      switch (stream) {
        case wire.stdout:
          this.capture.add(stream, body);
          this.handlers.onStdout?.(body);
          break;
        case wire.stderr:
          this.capture.add(stream, body);
          this.handlers.onStderr?.(body);
          break;
        case wire.exit:
          return wire.exitOf(body, this.what);
        case wire.failure:
          throw wire.failureOf(body, this.what);
        default:
          throw new ProtocolError(`${this.what}: the daemon sent a message of stream ${stream}, which no daemon sends`);
      }
    }

    return undefined;
  }

  /** cut names a stream that ended before its exit, with the record's word on why; the record is best effort, as the CLI's. */
  private async cut(dropped: unknown): Promise<ShardConnectionError> {
    const ended = `${this.what}: the stream ended without an exit status`;
    try {
      const record = await this.inspect();

      return new ShardConnectionError(`${ended}; the command is ${record.state}, with ${record.lostBytes} bytes of output lost`, { cause: dropped });
    } catch (err) {
      return new ShardConnectionError(ended, { cause: dropped ?? err });
    }
  }

  private path(): string {
    return wire.path("sandboxes", this.sandboxId, "exec", this.id);
  }

  private get params(): { path: { id: string; exec: string } } {
    return { path: { id: this.sandboxId, exec: this.id } };
  }
}

function message(stream: number, payload: Uint8Array): Buffer {
  return Buffer.concat([Buffer.of(stream), payload]);
}

export function commandInfo(value: unknown): CommandInfo {
  const refused = new ProtocolError(`the daemon answered ${JSON.stringify(value)} as a command record`);
  if (!isObject(value)) {
    throw refused;
  }
  const { exec, sandbox, command, state, exit_status: status, started_at: started, exited_at: exited = null, lost_bytes: lost = 0 } = value;
  const statusValid = status === null || (isObject(status) && Number.isInteger(status.code) && Number.isInteger(status.signal));
  const startedAt = date(started);
  const exitedAt = exited === null ? null : date(exited);
  if (
    typeof exec !== "string" ||
    typeof sandbox !== "string" ||
    !isStrings(command) ||
    (state !== "running" && state !== "exited") ||
    !statusValid ||
    !startedAt ||
    exitedAt === undefined ||
    !Number.isInteger(lost)
  ) {
    throw refused;
  }

  return {
    id: exec,
    sandboxId: sandbox,
    command,
    state,
    exitCode: isObject(status) ? Number(status.code) : null,
    signal: isObject(status) ? Number(status.signal) || null : null,
    startedAt,
    exitedAt,
    lostBytes: Number(lost),
  };
}
