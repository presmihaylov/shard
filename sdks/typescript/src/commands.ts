// The commands of one sandbox: a run answers how the command ended, a start answers a handle to it.
import { OutputCapture, defaultOutputLimit } from "./capture.js";
import { ProtocolError, isObject } from "./errors.js";
import { Session, commandInfo, type CommandInfo, type Handlers } from "./exec.js";
import type { Transport } from "./transport.js";
import * as wire from "./wire.js";

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
  /** The signal that ended the command, as 9 for KILL, or null for a normal exit. */
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
    const fed = input === undefined ? Promise.resolve() : feed(session, input);
    const [exit] = await Promise.all([session.wait(options.signal), fed]);

    return result(exit, capture);
  }

  /** start answers a handle once the command runs, with any stdin given already sent. */
  async start(command: string | string[], options: ExecOptions = {}): Promise<Command> {
    const { session, capture, input } = await this.open(command, options);
    if (input !== undefined) {
      await feed(session, input);
    }

    return new Command(session, capture);
  }

  /** list answers every command the daemon still holds for the sandbox, running or ended. */
  async list(): Promise<CommandInfo[]> {
    const commands: CommandInfo[] = [];
    let cursor = "";
    do {
      const page = await this.transport.call("GET", this.path(), { query: cursor ? { cursor } : {} });
      const execs = isObject(page) ? page.execs : undefined;
      const next = isObject(page) ? page.next : undefined;
      if (!Array.isArray(execs) || (next !== null && typeof next !== "string")) {
        throw new ProtocolError(`the daemon answered ${JSON.stringify(page)} as a page of commands`);
      }
      commands.push(...execs.map(commandInfo));
      cursor = next ?? "";
    } while (cursor);

    return commands;
  }

  /** get answers a handle to a command any client started; its output comes when it attaches. */
  async get(id: string, options: OutputOptions = {}): Promise<Command> {
    const info = commandInfo(await this.transport.call("GET", wire.path("sandboxes", this.sandboxId, "exec", id)));
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

  private path(): string {
    return wire.path("sandboxes", this.sandboxId, "exec");
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
    if (await session.endedWithExit()) {
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
