// The commands of one sandbox: a run answers how the command ended, a start answers a handle to it.
import { OutputCapture, defaultOutputLimit } from "./capture.js";
import { Session, commandInfo, type CommandInfo, type Handlers } from "./exec.js";
import { listed } from "./pages.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";
import { endedStream } from "./ws.js";

/** OutputOptions say where a command's output goes: every chunk to the callbacks, the newest bytes to the result. */
export interface OutputOptions {
  /** Gets every chunk of stdout as it arrives, the ones past the output limit too. */
  onStdout?: (chunk: Uint8Array) => void;
  onStderr?: (chunk: Uint8Array) => void;
  /** How many of the newest bytes of stdout and stderr together the result keeps; 0 keeps none. The default is 8 MiB. */
  outputLimitBytes?: number;
}

export interface ExecOptions extends OutputOptions {
  /** Variables added to the sandbox's environment for this command. */
  env?: Record<string, string>;
  workdir?: string;
  /** A user name or uid in the sandbox, as nobody or 1000. */
  user?: string;
  /** Text or bytes the command reads, after which its input ends; true leaves the input open for writeStdin. */
  stdin?: string | Uint8Array | boolean;
  /** Runs the command on a terminal of this size, which sends everything as stdout. */
  tty?: wire.TerminalSize;
  /** An abort ends this client's wait and lets go of the stream; the command runs on in the sandbox. */
  signal?: AbortSignal;
}

/** ExecResult is how a command ended, with the end of its output. */
export interface ExecResult {
  exitCode: number;
  /** The signal the server reported, or null; every provider reports a signal as the exit code 128 plus its number, as 137 for KILL. */
  signal: number | null;
  /** The newest output, at most outputLimitBytes of stdout and stderr together, so a long run keeps only its end. */
  stdout: string;
  stderr: string;
  /** Output the daemon dropped before any client read it, a gap no output limit of this client explains. */
  lostBytes: number;
}

/** Command is a handle to one command in a sandbox, which outlives every stream this client opens to it. */
export class Command {
  constructor(
    private readonly session: Session,
    private readonly capture: OutputCapture,
  ) {}

  get id(): string {
    return this.session.id;
  }

  get sandboxId(): string {
    return this.session.sandboxId;
  }

  inspect(): Promise<CommandInfo> {
    return this.session.inspect();
  }

  /** wait answers how the command ended. With no stream open it attaches, and the output starts again from the daemon's oldest byte. */
  async wait(options: { signal?: AbortSignal } = {}): Promise<ExecResult> {
    return result(await this.session.wait(options.signal), this.capture);
  }

  /** kill sends a signal by name, as KILL; the default is TERM. */
  kill(signal?: string): Promise<void> {
    return this.session.kill(signal);
  }

  resize(rows: number, cols: number): Promise<void> {
    return this.session.resize({ rows, cols });
  }

  writeStdin(data: string | Uint8Array): Promise<void> {
    return this.session.writeStdin(data);
  }

  closeStdin(): Promise<void> {
    return this.session.closeStdin();
  }

  /** disconnect lets go of the stream and leaves the command running. */
  disconnect(): void {
    this.session.disconnect();
  }

  /** reconnect opens a new stream, which replays the output the daemon still holds and streams the rest. */
  reconnect(options: { signal?: AbortSignal } = {}): Promise<void> {
    return this.session.attach(options.signal);
  }
}

export class Commands {
  constructor(
    private readonly transport: Transport,
    private readonly sandboxId: string,
  ) {}

  /** run answers how the command ended; a nonzero exit is a result, and a command that never started throws. */
  async run(command: string | string[], options: ExecOptions = {}): Promise<ExecResult> {
    const { session, capture, input } = await this.open(command, options);
    if (input === undefined) {
      return result(await session.wait(options.signal), capture);
    }
    // A failed feed ends the wait too, so the rejected run holds no stream open.
    const fedFailed = new AbortController();
    const signal = options.signal ? AbortSignal.any([options.signal, fedFailed.signal]) : fedFailed.signal;
    const fed = feed(session, input).catch((err: unknown) => {
      fedFailed.abort(err);
      throw err;
    });
    const [exit] = await Promise.all([session.wait(signal), fed]);

    return result(exit, capture);
  }

  /** start answers a handle once the command runs, with any stdin given already sent. */
  async start(command: string | string[], options: ExecOptions = {}): Promise<Command> {
    const { session, capture, input } = await this.open(command, options);
    if (input !== undefined) {
      // The caller gets no handle, so nothing else would ever let go of the stream.
      await feed(session, input).catch((err: unknown) => {
        session.disconnect();
        throw err;
      });
    }

    return new Command(session, capture);
  }

  /** list answers every command the daemon still holds for the sandbox, running or ended. */
  async list(): Promise<CommandInfo[]> {
    const path = { id: this.sandboxId };
    const route = "/v0/sandboxes/{id}/exec";
    const rows = await listed(route, "execs", (cursor) => this.transport.api.GET(route, { params: { path, query: { cursor } } }));

    return rows.map(commandInfo);
  }

  /** get answers a handle to a command any client started; its output comes when it attaches. */
  async get(id: string, options: OutputOptions = {}): Promise<Command> {
    const params = { path: { id: this.sandboxId, exec: id } };
    const info = commandInfo((await this.transport.api.GET("/v0/sandboxes/{id}/exec/{exec}", { params })).data);
    const capture = new OutputCapture(options.outputLimitBytes ?? defaultOutputLimit);

    return new Command(new Session(this.transport, this.sandboxId, info.id, capture, handlers(options)), capture);
  }

  private async open(command: string | string[], options: ExecOptions) {
    const capture = new OutputCapture(options.outputLimitBytes ?? defaultOutputLimit);
    const input = typeof options.stdin === "string" || options.stdin instanceof Uint8Array ? options.stdin : undefined;
    const { env, workdir, user, tty } = options;
    const request = wire.execRequest(command, { env, workdir, user, tty, stdin: options.stdin !== undefined && options.stdin !== false });
    const session = await Session.start(this.transport, this.sandboxId, request, capture, handlers(options), options.signal);

    return { session, capture, input };
  }
}

function handlers(options: OutputOptions): Handlers {
  return { onStdout: options.onStdout, onStderr: options.onStderr };
}

async function feed(session: Session, input: string | Uint8Array): Promise<void> {
  try {
    await session.writeStdin(input);
    await session.closeStdin();
  } catch (err) {
    if (endedStream(err) && (await session.endedWithExit())) {
      // @shard 2026-10-04: stdin closed by the command; wait() reports the exit
      return;
    }
    throw err;
  }
}

function result(exit: wire.Exit, capture: OutputCapture): ExecResult {
  const { stdout, stderr } = capture.output();

  return { ...exit, stdout: stdout.toString("utf8"), stderr: stderr.toString("utf8") };
}
