// The connection settings: an explicit option beats the environment, as the shard CLI reads them.
import { X509Certificate } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import { ConfigurationError, isObject } from "./errors.js";

export const remoteEnv = "SHARD_REMOTE";
export const apiKeyEnv = "SHARD_API_KEY";
export const tokenFileEnv = "SHARD_TOKEN_FILE";
export const caFileEnv = "SHARD_CA_FILE";

/** ShardOptions are the connection settings; each one left out is read from its environment variable. */
export interface ShardOptions {
  /** The https url of `shard serve`, as https://shard.example.com. The default is SHARD_REMOTE. */
  remote?: string;
  /** The API key. The default is SHARD_API_KEY, then the file SHARD_TOKEN_FILE names. */
  apiKey?: string;
  /** A file that holds the API key, or the JSON record `shard tokens mint` writes. */
  tokenFile?: string;
  /** A PEM file of the CA that signed the server's certificate. The default is SHARD_CA_FILE, then the system's CAs. */
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
  return {
    baseUrl: baseUrl(options.remote, env),
    apiKey: apiKey(options, env),
    ca: ca(options.caFile, env),
  };
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
    throw new ConfigurationError(`${source} must be an https url with a host, ${example}`);
  }
  if (url.protocol !== "https:" || !url.hostname) {
    throw new ConfigurationError(`${source} must be an https url with a host, ${example}`);
  }
  if (url.username || url.password || url.search || url.hash || url.pathname !== "/") {
    throw new ConfigurationError(`${source} must name only a scheme, a host and a port, ${example}`);
  }

  return `https://${url.hostname}:${url.port || "443"}`;
}

function apiKey(options: ShardOptions, env: Env): string {
  if (options.apiKey !== undefined) {
    return checked(options.apiKey.trim(), "apiKey");
  }
  if (options.tokenFile !== undefined) {
    return readTokenFile(options.tokenFile, "tokenFile");
  }
  // A blank key is unset, so a stray export never shadows SHARD_TOKEN_FILE.
  const key = (env[apiKeyEnv] ?? "").trim();
  if (key) {
    return checked(key, apiKeyEnv);
  }
  const path = env[tokenFileEnv] ?? "";
  if (path) {
    return readTokenFile(path, tokenFileEnv);
  }
  throw new ConfigurationError(`no API key: pass apiKey or tokenFile, or set ${apiKeyEnv} or ${tokenFileEnv}, in that order`);
}

function readTokenFile(path: string, source: string): string {
  const text = readTokenText(path, source);
  if (!text) {
    throw new ConfigurationError(`${source}: the token file ${path} holds no token`);
  }
  if (!text.startsWith("{")) {
    return checked(text, source);
  }
  // tokens mint writes a JSON record; a token file holds it whole or the bare token.
  let record: unknown;
  try {
    record = JSON.parse(text);
  } catch {
    throw new ConfigurationError(`${source}: the token file ${path} holds a record that is not JSON`);
  }
  const token = isObject(record) ? record.token : undefined;
  if (typeof token !== "string" || !token.trim()) {
    throw new ConfigurationError(`${source}: the record in the token file ${path} holds no token`);
  }

  return checked(token.trim(), source);
}

function readTokenText(path: string, source: string): string {
  let bytes: Buffer;
  try {
    const mode = statSync(path).mode & 0o777;
    if (mode & 0o007) {
      throw new ConfigurationError(`${source}: the token file ${path} is at mode ${mode.toString(8).padStart(4, "0")}, which others can read`);
    }
    bytes = readFileSync(path);
  } catch (err) {
    if (err instanceof ConfigurationError) {
      throw err;
    }
    throw new ConfigurationError(`${source}: read the token file ${path}: ${reason(err)}`);
  }
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes).trim();
  } catch {
    throw new ConfigurationError(`${source}: the token file ${path} is not UTF-8 text`);
  }
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

function ca(caFile: string | undefined, env: Env): Buffer | undefined {
  const source = caFile ? "caFile" : caFileEnv;
  const path = caFile || env[caFileEnv] || "";
  if (!path) {
    return undefined;
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
