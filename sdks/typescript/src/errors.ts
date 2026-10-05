// The errors the SDK throws. A nonzero exit is a result, never one of these.

// A body that is not the daemon's JSON, such as a proxy page, is quoted only this far.
const rawBodyLimit = 512;

/** ShardError is the base of every SDK error; a bad local argument throws the native error instead. */
export class ShardError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = new.target.name;
  }
}

/** ConfigurationError is a setting that is missing or refused. The message never shows the API key; it names a CA path or remote to fix. */
export class ConfigurationError extends ShardError {}

/** ShardConnectionError is a daemon that could not be reached, or a stream that ended before it said how the command ended. */
export class ShardConnectionError extends ShardError {}

/** ProtocolError is an answer from the daemon that the SDK cannot read. */
export class ProtocolError extends ShardError {}

/** UnknownLengthError is an upload whose size is unknown, or whose source changed size while it was sent; the daemon refuses a body without a Content-Length. */
export class UnknownLengthError extends ShardError {}

/** CommandNotStartedError is a command that never ran, as when its binary does not exist. */
export class CommandNotStartedError extends ShardError {
  constructor(
    readonly exitCode: number,
    readonly reason: string,
  ) {
    super(`the command did not start: ${reason}`);
  }
}

/** UnsafeArchiveError is a tar entry from a sandbox that downloadDir refuses to land, as one that leaves the destination. */
export class UnsafeArchiveError extends ShardError {
  constructor(
    readonly entry: string,
    readonly reason: string,
  ) {
    super(`refuse the entry ${JSON.stringify(entry)}: ${reason}`);
  }
}

/** APIError is a request the daemon refused. code is the daemon's error code, empty when the body held none. */
export class APIError extends ShardError {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly detail: string,
    readonly holders?: string[],
  ) {
    super(code ? `${status} ${code}: ${detail}` : `${status}: ${detail}`);
  }
}

/** AuthenticationError is a 401: the API key is missing, unknown or expired. */
export class AuthenticationError extends APIError {}

/** PermissionDeniedError is a 403: the key's scopes do not cover the route, or the route is local to the host. */
export class PermissionDeniedError extends APIError {}

/** NotFoundError is a 404: no such sandbox, snapshot, command, file, secret or policy. */
export class NotFoundError extends APIError {}

/** InvalidRequestError is a 400 or a 413: the daemon refused the request as written. */
export class InvalidRequestError extends APIError {}

/** ConflictError is a 409: the request does not fit the current state, as a snapshot of a running sandbox. */
export class ConflictError extends APIError {}

/** UnsupportedError is a verb the provider does not have. The message names the provider and the verb. */
export class UnsupportedError extends APIError {}

/** ServerError is a 5xx: the daemon or the provider failed. */
export class ServerError extends APIError {}

type APIErrorClass = new (status: number, code: string, detail: string, holders?: string[]) => APIError;

const byStatus: ReadonlyMap<number, APIErrorClass> = new Map([
  [400, InvalidRequestError],
  [401, AuthenticationError],
  [403, PermissionDeniedError],
  [404, NotFoundError],
  [409, ConflictError],
  [413, InvalidRequestError],
]);

// A stream failure carries a code and no status, so the status is the one the daemon answers that code with.
const codeStatus: ReadonlyMap<string, number> = new Map([
  ["invalid_request", 400],
  ["body_too_large", 413],
  ["unauthorized", 401],
  ["forbidden", 403],
  ["not_found", 404],
  ["sandbox_not_running", 409],
  ["sandbox_not_stopped", 409],
  ["sandbox_not_paused", 409],
  ["sandbox_live", 409],
  ["sandbox_failed", 409],
  ["no_checkpoint", 409],
  ["unsupported", 409],
  ["in_use", 409],
  ["name_taken", 409],
  ["exec_exited", 409],
  ["exec_running", 409],
  ["exec_limit", 429],
  ["no_app", 409],
  ["app_ended", 409],
  ["command_not_started", 422],
  ["timeout", 504],
  ["internal", 500],
]);

/** apiError classifies a refusal by the daemon's code, then by its status. */
export function apiError(status: number, body: Uint8Array): APIError | CommandNotStartedError {
  const { code, detail, exitCode, holders } = parseBody(body);
  if (code === "command_not_started" && exitCode !== undefined) {
    return new CommandNotStartedError(exitCode, detail);
  }

  return classified(status, code, detail, holders);
}

/** statusError classifies a refusal that came with no body, as the answer to a HEAD. */
export function statusError(status: number, detail: string): APIError {
  return classified(status, "", detail);
}

/** failureError classifies a failure the daemon sent on a stream after the handshake. */
export function failureError(code: string, detail: string): APIError {
  return classified(codeStatus.get(code) ?? 500, code, detail);
}

function classified(status: number, code: string, detail: string, holders?: string[]): APIError {
  if (code === "unsupported") {
    return new UnsupportedError(status, code, detail, holders);
  }
  const Class = byStatus.get(status) ?? (status >= 500 ? ServerError : APIError);

  return new Class(status, code, detail, holders);
}

function parseBody(body: Uint8Array): { code: string; detail: string; exitCode?: number; holders?: string[] } {
  let decoded: unknown;
  try {
    decoded = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
  } catch {
    return { code: "", detail: raw(body) };
  }
  const err = isObject(decoded) ? decoded.error : undefined;
  if (!isObject(err)) {
    return { code: "", detail: raw(body) };
  }

  return {
    code: typeof err.code === "string" ? err.code : "",
    detail: typeof err.message === "string" ? err.message : "",
    ...(typeof err.exit_code === "number" && Number.isInteger(err.exit_code) ? { exitCode: err.exit_code } : {}),
    ...(Array.isArray(err.holders) && err.holders.every((holder: unknown): holder is string => typeof holder === "string") ? { holders: err.holders } : {}),
  };
}

function raw(body: Uint8Array): string {
  const text = new TextDecoder().decode(body.subarray(0, rawBodyLimit)).trim();

  return text || "an empty body";
}

export function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
