import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { resolve } from "../src/config.js";
import { ConfigurationError } from "../src/errors.js";
import { certificate } from "./helpers/tls.js";

const dir = mkdtempSync(join(tmpdir(), "useshards-config-"));
after(() => rmSync(dir, { recursive: true, force: true }));

const remote = "https://shard.example.com";

function file(name: string, content: string | Buffer): string {
  const path = join(dir, name);
  writeFileSync(path, content);

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

test("the key comes from apiKey, then SHARD_API_KEY", () => {
  const env = { SHARD_REMOTE: remote, SHARD_API_KEY: "env-key" };
  assert.equal(resolve({ apiKey: "option-key" }, env).apiKey, "option-key");
  assert.equal(resolve({}, env).apiKey, "env-key");
  refused(() => resolve({}, { SHARD_REMOTE: remote }), /^no API key: pass apiKey or set SHARD_API_KEY$/);
  refused(() => resolve({}, { ...env, SHARD_API_KEY: "  " }), /^no API key/);
});

test("a key with a control character is refused, and the message never holds the key", () => {
  assert.throws(
    () => resolve({ remote, apiKey: "secret\rvalue" }),
    (err: unknown) => err instanceof ConfigurationError && err.message.includes("control character") && !err.message.includes("secret"),
  );
});

test("the remote is an http or https url that names only a host and a port", () => {
  const apiKey = "key";
  refused(() => resolve({ apiKey }, {}), /^no remote: pass remote or set SHARD_REMOTE/);
  refused(() => resolve({ apiKey, remote: "shard.example.com" }), /^remote must be an http or https url/);
  refused(() => resolve({ apiKey }, { SHARD_REMOTE: "ftp://x" }), /^SHARD_REMOTE must be an http or https url/);
  for (const remote of ["https://u:p@h.example.com", "https://h.example.com/v0", "https://h.example.com?a=1", "https://h.example.com#x"]) {
    refused(() => resolve({ apiKey, remote }), /^remote must name only a scheme, a host and a port/);
  }
  assert.equal(resolve({ apiKey, remote: "https://h.example.com/" }).baseUrl, "https://h.example.com:443");
  assert.equal(resolve({ apiKey, remote: "http://h.example.com" }).baseUrl, "http://h.example.com:80");
  assert.equal(resolve({ apiKey, remote: "http://h.example.com:8080" }).baseUrl, "http://h.example.com:8080");
});

test("a CA file must hold a certificate", () => {
  const pem = certificate().cert;
  assert.deepEqual(resolve({ remote, apiKey: "key", caFile: file("ca.pem", pem) }).ca, pem);
  assert.deepEqual(resolve({ apiKey: "key" }, { SHARD_REMOTE: remote, SHARD_CA_FILE: file("env-ca.pem", pem) }).ca, pem);
  refused(() => resolve({ remote, apiKey: "key", caFile: file("junk.pem", "not a certificate") }), /holds no certificate/);
  refused(() => resolve({ remote, apiKey: "key", caFile: join(dir, "no-ca.pem") }), /^caFile: read the CA file/);
});

test("a CA file with an http remote is refused before it is read", () => {
  const plain = "http://shard.example.com";
  const missing = join(dir, "never-read.pem");
  const words = (source: string) => new RegExp(`^${source} is set, and the remote http://shard.example.com:80 is http: a CA certificate verifies an https remote only$`);
  refused(() => resolve({ remote: plain, apiKey: "key", caFile: missing }), words("caFile"));
  refused(() => resolve({ apiKey: "key" }, { SHARD_REMOTE: plain, SHARD_CA_FILE: missing }), words("SHARD_CA_FILE"));
});
