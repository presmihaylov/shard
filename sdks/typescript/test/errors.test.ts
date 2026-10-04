import assert from "node:assert/strict";
import { test } from "node:test";
import {
  APIError,
  AuthenticationError,
  ConflictError,
  InvalidRequestError,
  NotFoundError,
  PermissionDeniedError,
  ServerError,
  ShardError,
  UnsupportedError,
  apiError,
  failureError,
} from "../src/errors.js";

const body = (code: string, message: string) => Buffer.from(JSON.stringify({ error: { code, message } }));

test("a refusal is classed by its status", () => {
  const cases: Array<[number, new (...args: never[]) => APIError]> = [
    [400, InvalidRequestError],
    [401, AuthenticationError],
    [403, PermissionDeniedError],
    [404, NotFoundError],
    [409, ConflictError],
    [413, InvalidRequestError],
    [500, ServerError],
    [504, ServerError],
  ];
  for (const [status, Class] of cases) {
    const err = apiError(status, body("some_code", "the words"));
    assert.ok(err instanceof Class, `${status} is ${err.name}`);
    assert.ok(err instanceof ShardError);
    assert.equal(err.name, Class.name);
    assert.equal(err.status, status);
    assert.equal(err.code, "some_code");
    assert.equal(err.detail, "the words");
    assert.equal(err.message, `${status} some_code: the words`);
  }
});

test("unsupported wins over its status", () => {
  const err = apiError(409, body("unsupported", "sysbox does not pause"));
  assert.ok(err instanceof UnsupportedError);
  assert.ok(err instanceof APIError);
});

test("a status with no class is a plain APIError", () => {
  const err = apiError(418, body("teapot", "short and stout"));
  assert.equal(err.constructor, APIError);
});

test("a body that is not the daemon's JSON is quoted, short", () => {
  const err = apiError(502, Buffer.from(`  <html>${"x".repeat(2000)}</html>  `));
  assert.ok(err instanceof ServerError);
  assert.equal(err.code, "");
  assert.ok(err.detail.startsWith("<html>"));
  assert.equal(err.detail.length, 510, "512 bytes, trimmed of the two leading spaces");
  assert.equal(err.message, `502: ${err.detail}`);
  assert.equal(refused(502, new Uint8Array(0)).detail, "an empty body");
  assert.equal(refused(400, Buffer.from('{"error":"flat"}')).detail, '{"error":"flat"}');
  assert.equal(refused(400, Buffer.from([0xff, 0xfe])).code, "");
});

function refused(status: number, body: Uint8Array): APIError {
  const err = apiError(status, body);
  assert.ok(err instanceof APIError);

  return err;
}

test("a stream failure takes the status its code answers with", () => {
  const cases: Array<[string, number, new (...args: never[]) => APIError]> = [
    ["not_found", 404, NotFoundError],
    ["exec_exited", 409, ConflictError],
    ["unsupported", 409, UnsupportedError],
    ["substrate_timeout", 504, ServerError],
    ["body_too_large", 413, InvalidRequestError],
    ["new_code", 500, ServerError],
  ];
  for (const [code, status, Class] of cases) {
    const err = failureError(code, "why");
    assert.ok(err instanceof Class, `${code} is ${err.name}`);
    assert.equal(err.status, status);
  }
});
