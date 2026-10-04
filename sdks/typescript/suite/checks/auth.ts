import assert from "node:assert/strict";
import { AuthenticationError, ConfigurationError, Shard } from "useshards";
import { raw, rejects, type Check } from "../harness.js";

// The local routes 488 keeps off serve; a probe that would change the daemon goes last, once every other one was refused.
const localProbes: Array<[string, string]> = [
  ["GET", "/v0/daemon"],
  ["GET", "/v0/local/sandboxes"],
  ["GET", "/v0/local/sandboxes/suite-ts-no-such-sandbox"],
  ["GET", "/v0/images"],
  ["DELETE", "/v0/images/suite-ts-no-such-image:none"],
  ["POST", "/v0/images/pull"],
  ["POST", "/v0/images/prune"],
];

// A scoped key is refused a local route on main too, so only a "*" key proves serve keeps the route local.
const wildcardKeyEnv = "SHARD_SUITE_WILDCARD_KEY";

/** withEnv runs with the given variables in place of the real ones and puts the real ones back. */
async function withEnv(env: Record<string, string | undefined>, run: () => Promise<void>): Promise<void> {
  const saved = Object.fromEntries(Object.keys(env).map((key) => [key, process.env[key]]));
  const assign = (values: Record<string, string | undefined>) => {
    for (const [key, value] of Object.entries(values)) {
      if (value === undefined) {
        delete process.env[key];
        continue;
      }
      process.env[key] = value;
    }
  };
  assign(env);
  try {
    await run();
  } finally {
    assign(saved);
  }
}

export const checks: Check[] = [
  {
    name: "auth.env_defaults",
    run: async () => {
      const shard = new Shard();
      assert.ok(Array.isArray((await shard.list()).sandboxes), "a client from SHARD_REMOTE and SHARD_API_KEY lists sandboxes");
    },
  },
  {
    name: "auth.explicit_overrides",
    run: async () => {
      const remote = process.env.SHARD_REMOTE;
      const apiKey = process.env.SHARD_API_KEY;
      await withEnv({ SHARD_REMOTE: "https://shard.invalid", SHARD_API_KEY: "shard_not_a_key" }, async () => {
        assert.ok(Array.isArray((await new Shard({ remote, apiKey }).list()).sandboxes), "explicit settings win over bad env");
      });
      await rejects(AuthenticationError, () => new Shard({ apiKey: "shard_not_a_key" }).list());
    },
  },
  {
    name: "auth.no_key",
    run: async () => {
      await withEnv({ SHARD_API_KEY: undefined }, async () => {
        await rejects(ConfigurationError, async () => new Shard().list());
      });
    },
  },
  {
    name: "auth.bad_key",
    run: async () => {
      const err = await rejects(AuthenticationError, () => new Shard({ apiKey: "shard_not_a_key" }).list());
      assert.equal(err.status, 401);
    },
  },
  {
    name: "auth.local_routes_refused",
    run: async () => {
      const wildcard = process.env[wildcardKeyEnv];
      assert.ok(wildcard, `${wildcardKeyEnv} is not set: it must hold a "*" token`);
      const keys: Array<[string, string]> = [
        ["SHARD_API_KEY", process.env.SHARD_API_KEY ?? ""],
        [wildcardKeyEnv, wildcard],
      ];
      let refusal: string | undefined;
      for (const [method, path] of localProbes) {
        for (const [name, key] of keys) {
          const got = await raw(method, path, key);
          assert.equal(got.status, 403, `${method} ${path} with ${name} answered ${got.status} ${got.body}`);
          // Every local route reads like an unknown one, so the body is the same for all of them.
          refusal ??= got.body;
          assert.equal(got.body, refusal, `${method} ${path} with ${name} answered a different refusal`);
        }
      }
      const unknown = await raw("GET", "/v0/suite-ts-no-such-route");
      assert.equal(unknown.status, 403);
      assert.equal(unknown.body, refusal, "a local route is refused like an unknown route");
    },
  },
  {
    name: "auth.version_public",
    run: async ({ shard }) => {
      const version = await shard.version();
      assert.ok(version.version.length > 0, "version names the daemon version");
      const got = await raw("GET", "/v0/version");
      assert.equal(got.status, 200);
      const keys = Object.keys(JSON.parse(got.body)).filter((key) => key !== "version" && key !== "api_version");
      assert.deepEqual(keys, [], "the public version holds nothing but versions");
    },
  },
];
