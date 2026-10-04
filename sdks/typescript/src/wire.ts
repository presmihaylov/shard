// What every call shares on the wire: paths, the command stream ids, and the messages that end a stream.
import { CommandNotStartedError, ProtocolError, failureError, isObject, type APIError } from "./errors.js";
import type { components } from "./generated/schema.js";

// The stream a command message carries in its first byte: the client sends stdin and stdin closed, the daemon the rest.
export const stdin = 0;
export const stdout = 1;
export const stderr = 2;
export const exit = 3;
export const stdinClose = 4;
export const failure = 5;

export type OutputStream = typeof stdout | typeof stderr;

// The daemon reads one stream id and at most this much after it in each message.
export const maxPayload = 1024 * 1024;

export function path(...parts: string[]): string {
  return `/v0/${parts.map(encodeURIComponent).join("/")}`;
}

/** query keeps the values a call sets; a flag goes as true, the spelling the daemon's ParseBool takes. */
export function query(values: Record<string, string | number | boolean | undefined>): Record<string, string> {
  const set: Record<string, string> = {};
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== false && value !== "") {
      set[key] = String(value);
    }
  }

  return set;
}

/** TerminalSize is the window of a command that runs on a terminal. */
export interface TerminalSize {
  rows: number;
  cols: number;
}

/** ExecRequest is what a command start says, as the daemon reads it. */
export type ExecRequest = components["schemas"]["ExecRequest"];

export interface ExecSettings {
  env?: Record<string, string>;
  workdir?: string;
  user?: string;
  stdin: boolean;
  tty?: TerminalSize;
}

/** execRequest builds a command start. A string runs under /bin/sh -c, an array as the argv itself. */
export function execRequest(command: string | string[], settings: ExecSettings): ExecRequest {
  const argv = typeof command === "string" ? ["/bin/sh", "-c", command] : [...command];
  if (argv.length === 0) {
    throw new TypeError("command must name a program");
  }
  // The attach follows at once, so the daemon keeps the output for it rather than evict any.
  const request: ExecRequest = { command: argv, stdin: settings.stdin, tty: settings.tty !== undefined, attach: true };
  if (settings.env && Object.keys(settings.env).length > 0) {
    request.env = Object.entries(settings.env).map(([key, value]) => `${key}=${value}`);
  }
  if (settings.workdir) {
    request.workdir = settings.workdir;
  }
  if (settings.user) {
    request.user = settings.user;
  }
  if (settings.tty) {
    request.size = settings.tty;
  }

  return request;
}

/** Exit is how a command ended. lostBytes counts output the daemon evicted before any client read it. */
export interface Exit {
  exitCode: number;
  signal: number | null;
  lostBytes: number;
}

/** exitOf reads an exit message; one that names an error is a command the sandbox never ran. */
export function exitOf(payload: Uint8Array, what: string): Exit {
  const message = object(payload, `the exit of ${what}`);
  const { code, signal = 0, lost_bytes: lost = 0, error = "" } = message;
  if (!Number.isInteger(code) || !Number.isInteger(signal) || !Number.isInteger(lost) || typeof error !== "string") {
    throw new ProtocolError(`the daemon answered ${quote(payload)} as the exit of ${what}`);
  }
  if (error) {
    throw new CommandNotStartedError(Number(code), error);
  }

  return { exitCode: Number(code), signal: Number(signal) || null, lostBytes: Number(lost) };
}

/** failureOf reads a failure message into the error a refusal before the handshake would have been. */
export function failureOf(payload: Uint8Array, what: string): APIError {
  const err = object(payload, `the failure of ${what}`).error;
  const code = isObject(err) ? err.code : undefined;
  const message = isObject(err) ? err.message : undefined;
  if (typeof code !== "string" || typeof message !== "string" || !message) {
    throw new ProtocolError(`the daemon answered ${quote(payload)} as the failure of ${what}`);
  }

  return failureError(code, message);
}

function object(payload: Uint8Array, what: string): Record<string, unknown> {
  let decoded: unknown;
  try {
    decoded = JSON.parse(new TextDecoder().decode(payload));
  } catch {
    throw new ProtocolError(`the daemon answered ${quote(payload)} as ${what}`);
  }
  if (!isObject(decoded)) {
    throw new ProtocolError(`the daemon answered ${quote(payload)} as ${what}`);
  }

  return decoded;
}

// A broken peer can send a megabyte, so an error quotes only the start of it.
function quote(payload: Uint8Array): string {
  return JSON.stringify(new TextDecoder().decode(payload.subarray(0, 200)));
}
