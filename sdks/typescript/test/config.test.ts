import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { resolve } from "../src/config.js";
import { ConfigurationError } from "../src/errors.js";
import { certificate } from "./helpers/tls.js";

const dir = mkdtempSync(join(tmpdir(), "useshards-config-"));
after(() => rmSync(dir, { recursive: true, force: true }));

const remote = "https://shard.example.com";

function file(name: string, content: string | Buffer, mode = 0o600): string {
  const path = join(dir, name);
  writeFileSync(path, content);
  chmodSync(path, mode);

  return path;
}

function refused(run: () => unknown, words: RegExp): void {
  assert.throws(run, (err: unknown) => err instanceof ConfigurationError && words.test(err.message));
}

test("the environment fills what the options leave out", () => {
  const settings = resolve({}, { SHARD_REMOTE: remote, SHARD_API_KEY: " key-from-env\n" });
  assert.deepEqual(settings, { baseUrl: "https://shard.example.com:443", apiKey: "key-from-env", ca: undefined });
});

test("an explicit option beats its variable", () => {
  const env = { SHARD_REMOTE: "https://env.example.com", SHARD_API_KEY: "env-key" };
  const settings = resolve({ remote: "https://explicit.example.com:8443", apiKey: "explicit-key" }, env);
  assert.equal(settings.baseUrl, "https://explicit.example.com:8443");
  assert.equal(settings.apiKey, "explicit-key");
});

test("the key comes from apiKey, tokenFile, SHARD_API_KEY, then SHARD_TOKEN_FILE", () => {
  const optionFile = file("option-token", "option-file-key\n");
  const envFile = file("env-token", "env-file-key");
  const env = { SHARD_REMOTE: remote, SHARD_API_KEY: "env-key", SHARD_TOKEN_FILE: envFile };
  assert.equal(resolve({ apiKey: "option-key", tokenFile: optionFile }, env).apiKey, "option-key");
  assert.equal(resolve({ tokenFile: optionFile }, env).apiKey, "option-file-key");
  assert.equal(resolve({}, env).apiKey, "env-key");
  assert.equal(resolve({}, { ...env, SHARD_API_KEY: "  " }).apiKey, "env-file-key", "a blank key is unset");
  refused(() => resolve({}, { SHARD_REMOTE: remote }), /^no API key: pass apiKey or tokenFile/);
});

test("a token file may hold the record tokens mint writes", () => {
  const record = file("record", JSON.stringify({ id: "tok_1", token: "record-key", scopes: ["*"] }));
  assert.equal(resolve({ remote, tokenFile: record }).apiKey, "record-key");
  refused(() => resolve({ remote, tokenFile: file("no-token", '{"id":"tok_1"}') }), /holds no token/);
  refused(() => resolve({ remote, tokenFile: file("broken", "{not json") }), /not JSON/);
});

test("a token file others can read is refused, and so is one that is not there", () => {
  const open = file("open", "key", 0o644);
  refused(() => resolve({ remote, tokenFile: open }), /is at mode 0644, which others can read/);
  refused(() => resolve({ remote, tokenFile: join(dir, "missing") }), /read the token file .*missing: no such file or directory$/);
  refused(() => resolve({ remote, tokenFile: file("empty", "\n") }), /holds no token/);
  refused(() => resolve({ remote, tokenFile: file("binary", Buffer.from([0xff, 0xfe])) }), /not UTF-8 text/);
});

test("a key with a control character is refused, and the message never holds the key", () => {
  assert.throws(
    () => resolve({ remote, apiKey: "secret\rvalue" }),
    (err: unknown) => err instanceof ConfigurationError && err.message.includes("control character") && !err.message.includes("secret"),
  );
});

test("the remote is an https url that names only a host and a port", () => {
  const apiKey = "key";
  refused(() => resolve({ apiKey }, {}), /^no remote: pass remote or set SHARD_REMOTE/);
  refused(() => resolve({ apiKey, remote: "http://shard.example.com" }), /^remote must be an https url/);
  refused(() => resolve({ apiKey, remote: "shard.example.com" }), /^remote must be an https url/);
  refused(() => resolve({ apiKey }, { SHARD_REMOTE: "ftp://x" }), /^SHARD_REMOTE must be an https url/);
  for (const remote of ["https://u:p@h.example.com", "https://h.example.com/v0", "https://h.example.com?a=1", "https://h.example.com#x"]) {
    refused(() => resolve({ apiKey, remote }), /^remote must name only a scheme, a host and a port/);
  }
  assert.equal(resolve({ apiKey, remote: "https://h.example.com/" }).baseUrl, "https://h.example.com:443");
});

test("a CA file must hold a certificate", () => {
  const pem = certificate().cert;
  assert.deepEqual(resolve({ remote, apiKey: "key", caFile: file("ca.pem", pem) }).ca, pem);
  assert.deepEqual(resolve({ apiKey: "key" }, { SHARD_REMOTE: remote, SHARD_CA_FILE: file("env-ca.pem", pem) }).ca, pem);
  refused(() => resolve({ remote, apiKey: "key", caFile: file("junk.pem", "not a certificate") }), /holds no certificate/);
  refused(() => resolve({ remote, apiKey: "key", caFile: join(dir, "no-ca.pem") }), /^caFile: read the CA file/);
});
