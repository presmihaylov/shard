import assert from "node:assert/strict";
import { test } from "node:test";
import { ProtocolError } from "../src/errors.js";
import { MessageReader, acceptFor, encodeClose, encodeFrame, messageLimit, opBinary, opClose, opContinuation, opPing, opText } from "../src/frames.js";
import { decodeClientFrames, serverFrame } from "./helpers/daemon.js";

test("the accept key is the RFC 6455 example's", () => {
  assert.equal(acceptFor("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=");
});

test("a client frame is masked, and unmasks to its payload at every length", () => {
  for (const length of [0, 1, 125, 126, 65535, 65536, 200_000]) {
    const payload = Buffer.alloc(length, 7);
    const frame = encodeFrame(opBinary, payload);
    assert.ok((frame[1] ?? 0) & 0x80, "the mask bit is set");
    const [decoded] = decodeClientFrames(frame);
    assert.ok(decoded);
    assert.equal(decoded.opcode, opBinary);
    assert.deepEqual(decoded.payload, payload);
  }
});

test("a close carries its code", () => {
  const [close] = decodeClientFrames(encodeClose(1000));
  assert.ok(close);
  assert.equal(close.opcode, opClose);
  assert.equal(close.payload.readUInt16BE(0), 1000);
});

test("the reader joins fragments and bytes split anywhere", () => {
  const wire = Buffer.concat([
    serverFrame(opText, Buffer.from("hel"), false),
    serverFrame(opPing, Buffer.from("p")),
    serverFrame(opContinuation, Buffer.from("lo")),
    serverFrame(opBinary, Buffer.alloc(70_000, 1)),
  ]);
  for (const step of [1, 3, 1000, wire.length]) {
    const reader = new MessageReader();
    const messages = [];
    for (let at = 0; at < wire.length; at += step) {
      messages.push(...reader.feed(wire.subarray(at, at + step)));
    }
    assert.deepEqual(
      messages.map((m) => [m.opcode, m.payload.length]),
      [
        [opPing, 1],
        [opText, 5],
        [opBinary, 70_000],
      ],
    );
    assert.equal(messages[1]?.payload.toString(), "hello");
  }
});

test("the reader refuses what no daemon sends", () => {
  const cases: Array<[string, Buffer]> = [
    ["reserved bit", Buffer.from([0xc2, 0x00])],
    ["masked", Buffer.from([0x82, 0x80, 0, 0, 0, 0])],
    ["fragmented control", serverFrame(opPing, Buffer.alloc(0), false)],
    ["long control", serverFrame(opPing, Buffer.alloc(126))],
    ["unknown opcode", serverFrame(0x3, Buffer.alloc(0))],
    ["stray continuation", serverFrame(opContinuation, Buffer.alloc(0))],
    ["message inside another", Buffer.concat([serverFrame(opBinary, Buffer.alloc(1), false), serverFrame(opText, Buffer.alloc(1))])],
    ["frame over the limit", Buffer.from([0x82, 127, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff])],
  ];
  for (const [name, bytes] of cases) {
    assert.throws(() => new MessageReader().feed(bytes), ProtocolError, name);
  }
});

test("fragments over the limit together are refused", () => {
  const reader = new MessageReader();
  const half = Buffer.alloc(messageLimit / 2 + 1);
  reader.feed(serverFrame(opBinary, half, false));
  assert.throws(() => reader.feed(serverFrame(opContinuation, half)), ProtocolError);
});
