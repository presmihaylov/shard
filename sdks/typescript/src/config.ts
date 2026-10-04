// The connection settings: an explicit option beats the environment, as the shard CLI reads them.
import { X509Certificate } from "node:crypto";
import { readFileSync } from "node:fs";
import { ConfigurationError } from "./errors.js";

export const remoteEnv = "SHARD_REMOTE";
export const apiKeyEnv = "SHARD_API_KEY";
export const caFileEnv = "SHARD_CA_FILE";

/** plainWarning is the CLI's line for an http remote; Node prints the warning's name, Warning, before it. */
export const plainWarning = "HTTP does not encrypt this connection. Use it only on localhost or through a trusted encrypted network.";

/** ShardOptions are the connection settings; each one left out is read from its environment variable. */
export interface ShardOptions {
  /** The url of `shard serve`, as https://shard.example.com; http sends the key unencrypted. The default is SHARD_REMOTE. */
  remote?: string;
  /** The API key, as `shard tokens mint` prints it. The default is SHARD_API_KEY. */
  apiKey?: string;
  /** A PEM file of the CA that signed the server's certificate, for https only. The default is SHARD_CA_FILE, then the system's CAs. */
  caFile?: string;
}

/** Settings are the resolved options. They hold the key, so nothing prints them. */
export interface Settings {
  baseUrl: string;
  apiKey: string;
  ca: Buffer | undefined;
}

type Env = Readonly<Record<string, string | undefined>>;

export function resolve(options: ShardOptions, env: Env = process.env): Settings {
  const remote = baseUrl(options.remote, env);

  return { baseUrl: remote, apiKey: apiKey(options, env), ca: ca(options.caFile, env, remote) };
}

const example = "as https://shard.example.com";

function baseUrl(remote: string | undefined, env: Env): string {
  const source = remote ? "remote" : remoteEnv;
  const value = remote || env[remoteEnv] || "";
  if (!value) {
    throw new ConfigurationError(`no remote: pass remote or set ${remoteEnv}, ${example}`);
  }
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new ConfigurationError(`${source} must be an http or https url with a host, ${example}`);
  }
  if ((url.protocol !== "https:" && url.protocol !== "http:") || !url.hostname) {
    throw new ConfigurationError(`${source} must be an http or https url with a host, ${example}`);
  }
  if (url.username || url.password || url.search || url.hash || url.pathname !== "/") {
    throw new ConfigurationError(`${source} must name only a scheme, a host and a port, ${example}`);
  }

  return `${url.protocol}//${url.hostname}:${url.port || (url.protocol === "http:" ? "80" : "443")}`;
}

function apiKey(options: ShardOptions, env: Env): string {
  if (options.apiKey !== undefined) {
    return checked(options.apiKey.trim(), "apiKey");
  }
  const key = (env[apiKeyEnv] ?? "").trim();
  if (!key) {
    throw new ConfigurationError(`no API key: pass apiKey or set ${apiKeyEnv}`);
  }

  return checked(key, apiKeyEnv);
}

function checked(token: string, source: string): string {
  if (!token) {
    throw new ConfigurationError(`${source} is empty`);
  }
  if (/[\x00-\x08\x0a-\x1f\x7f]/.test(token)) {
    throw new ConfigurationError(`${source} holds a control character, which no HTTP header carries`);
  }

  return token;
}

function ca(caFile: string | undefined, env: Env, remote: string): Buffer | undefined {
  const source = caFile ? "caFile" : caFileEnv;
  const path = caFile || env[caFileEnv] || "";
  if (!path) {
    return undefined;
  }
  if (remote.startsWith("http:")) {
    throw new ConfigurationError(`${source} is set, and the remote ${remote} is http: a CA certificate verifies an https remote only`);
  }
  let pem: Buffer;
  try {
    pem = readFileSync(path);
  } catch (err) {
    throw new ConfigurationError(`${source}: read the CA file ${path}: ${reason(err)}`);
  }
  // Node takes a CA file of junk without a word and fails every handshake later, so the file is parsed here.
  try {
    new X509Certificate(pem);
  } catch {
    throw new ConfigurationError(`${source}: the CA file ${path} holds no certificate`);
  }

  return pem;
}

/** reason is the system's words for a failed file call, without the path Node repeats in its message. */
function reason(err: unknown): string {
  if (!(err instanceof Error)) {
    return String(err);
  }
  const words = /^[A-Z]+: ([^,]+)/.exec(err.message);

  return words?.[1] ?? err.message;
}
