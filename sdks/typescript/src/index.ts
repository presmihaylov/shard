export type { ShardOptions } from "./config.js";
export { defaultOutputLimit } from "./capture.js";
export { Command, Commands, type ExecOptions, type ExecResult, type OutputOptions } from "./commands.js";
export type { CommandInfo, CommandState } from "./exec.js";
export type { TerminalSize } from "./wire.js";
export {
  APIError,
  AuthenticationError,
  CommandNotStartedError,
  ConfigurationError,
  ConflictError,
  ConnectionError,
  InvalidRequestError,
  NotFoundError,
  PermissionDeniedError,
  ProtocolError,
  ServerError,
  ShardError,
  UnknownLengthError,
  UnsafeArchiveError,
  UnsupportedError,
} from "./errors.js";
export { version } from "./version.js";
