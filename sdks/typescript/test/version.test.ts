import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { version } from "../src/version.js";

test("the version is the one package.json publishes", () => {
  const pkg: unknown = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  assert.ok(typeof pkg === "object" && pkg !== null && "version" in pkg);
  assert.equal(pkg.version, version);
});
