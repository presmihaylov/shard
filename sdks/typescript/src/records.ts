// The records the daemon answers about sandboxes, apps, ports, policies, secrets and snapshots, read into camelCase.
import { Fields } from "./decode.js";
import { ProtocolError } from "./errors.js";

export type SandboxState = "pending" | "created" | "running" | "paused" | "unresponsive" | "stopped" | "failed";

const states: readonly SandboxState[] = ["pending", "created", "running", "paused", "unresponsive", "stopped", "failed"];

export type RestartPolicy = "no" | "on-failure" | "always";

const restartPolicies: readonly RestartPolicy[] = ["no", "on-failure", "always"];

/** ExitStatus is how an app ended: its exit code, and the signal that ended it or null. */
export interface ExitStatus {
  exitCode: number;
  signal: number | null;
}

export interface Resources {
  /** 0 is no bound. */
  memoryMiB: number;
  vcpus: number;
  diskMiB: number;
}

/** Restart says when the daemon starts an app again; backoff is the seconds between two starts. */
export interface Restart {
  policy: RestartPolicy;
  /** How many starts again on-failure allows; always takes none. */
  retries?: number;
  backoff?: number;
}

/** RestartInfo is the policy of an app and what it has done so far. */
export interface RestartInfo {
  policy: RestartPolicy;
  retries: number;
  backoff: number;
  /** How many times the app started again. */
  count: number;
  lastAt: Date | null;
  /** The retries ran out. */
  gaveUp: boolean;
  /** No start again will follow, as after a stop or a give-up. */
  ended: boolean;
}

/** AppInfo is the one app a run started in its sandbox. */
export interface AppInfo {
  command: string[];
  /** null while the app runs. */
  exitStatus: ExitStatus | null;
  /** null for a run with no restart policy. */
  restart: RestartInfo | null;
}

/** AppExit is how an app ended once its restart policy has nothing left to do. */
export interface AppExit {
  exitCode: number;
  signal: number | null;
  restarts: number;
}

/** SandboxInfo is one sandbox as the daemon holds it. */
export interface SandboxInfo {
  id: string;
  name: string | null;
  image: string;
  digest: string | null;
  /** The snapshot the sandbox was made from. */
  snapshot: string | null;
  /** The id of the sandbox this one was forked from. */
  forkedFrom: string | null;
  provider: string;
  /** The tag of the guest kernel a microVM last booted; null on a container substrate. */
  kernel: string | null;
  state: SandboxState;
  stoppedReason: string | null;
  failedReason: string | null;
  resources: Resources;
  /** null for a sandbox that create made, which runs no app. */
  app: AppInfo | null;
  /** The names of the secrets granted to the sandbox. */
  secrets: string[];
  policy: string | null;
  /** The host ports the sandbox forwards, whether or not they listen now. */
  ports: PortForward[];
  startedAt: Date | null;
  createdAt: Date;
}

/** PortForward carries a port on the host to a port on the sandbox's 127.0.0.1; public listens on 0.0.0.0 instead of the host's 127.0.0.1. */
export interface PortForward {
  hostPort: number;
  guestPort: number;
  public: boolean;
}

/** Port is one forward as the host serves it now. */
export interface Port {
  /** The sandbox id. */
  sandbox: string;
  sandboxName: string | null;
  hostPort: number;
  guestPort: number;
  public: boolean;
  /** What the listener binds: 127.0.0.1, or 0.0.0.0 for a public forward. */
  address: string;
  /** False while the sandbox is not running, or while the host refuses the port. */
  listening: boolean;
  /** Why a running sandbox's port does not listen, or else what the last connection hit. */
  error: string | null;
  /** The IPv4 addresses of the host a client reaches the port on while it listens. */
  reachableOn: HostAddress[];
}

/** HostAddress is one IPv4 address of the host, and the interface that holds it. */
export interface HostAddress {
  interface: string;
  address: string;
}

/** PolicyRule is one rule as `shard policy create` takes it: an action, allow or deny, and a rule, as `suffix:example.com tcp:443`. */
export interface PolicyRule {
  action: "allow" | "deny";
  rule: string;
}

const actions: readonly PolicyRule["action"][] = ["allow", "deny"];

export interface Policy {
  name: string;
  rules: PolicyRule[];
  /** The sandboxes the policy is attached to; null on a list, which leaves it out. */
  holders: string[] | null;
  /** Whether the rules let the sandbox resolve names; null on a list. */
  dns: DNSMode | null;
}

export type DNSMode = "open" | "closed";

const dnsModes: readonly DNSMode[] = ["open", "closed"];

/** SecretInfo is a secret as the daemon lists it, which never holds its value. */
export interface SecretInfo {
  name: string;
  /** The hosts the secret's value goes to. */
  destinations: string[];
  /** What the sandbox holds in the value's place. */
  placeholder: string;
  updatedAt: Date;
}

export interface Snapshot {
  id: string;
  name: string | null;
  /** The id of the sandbox it was taken of, which it outlives. */
  source: string;
  sourceName: string | null;
  image: string;
  digest: string;
  provider: string;
  diskMiB: number;
  memoryMiB: number;
  /** The bytes it takes on the host. */
  size: number;
  createdAt: Date;
}

/** EgressDecision is one egress decision: rule is the id of the rule that decided it, or why none did, as default; ruleText is that rule as the CLI spells it. */
export interface EgressDecision {
  time: Date;
  source: "proxy" | "host" | "dns";
  verdict: "allow" | "deny";
  host: string | null;
  port: number | null;
  address: string | null;
  rule: string;
  ruleText: string | null;
  reason: string | null;
}

const sources: readonly EgressDecision["source"][] = ["proxy", "host", "dns"];

export interface Version {
  version: string;
  apiVersion: string;
}

/** Capabilities say which of the nine lifecycle verbs the daemon's provider supports; snapshot is the creation of a filesystem snapshot, and port the forward of a host port. */
export interface Capabilities {
  create: boolean;
  start: boolean;
  stop: boolean;
  remove: boolean;
  pause: boolean;
  resume: boolean;
  fork: boolean;
  snapshot: boolean;
  port: boolean;
}

export function sandboxInfo(value: unknown): SandboxInfo {
  const fields = Fields.of(value, "a sandbox");
  const resources = fields.object("resources");
  const command = fields.strings("command");

  return {
    id: fields.string("id"),
    name: fields.optionalString("name"),
    image: fields.string("image"),
    digest: fields.optionalString("digest"),
    snapshot: fields.optionalString("snapshot"),
    forkedFrom: fields.optionalString("forked_from"),
    provider: fields.string("provider"),
    kernel: fields.optionalString("kernel"),
    state: fields.oneOf("state", states),
    stoppedReason: fields.optionalString("stopped_reason"),
    failedReason: fields.optionalString("failed_reason"),
    resources: { memoryMiB: resources.int("memory_mib"), vcpus: resources.int("vcpus"), diskMiB: resources.int("disk_mib") },
    app: command.length === 0 ? null : { command, exitStatus: exitStatus(fields.optionalObject("exit_status")), restart: restart(fields) },
    secrets: fields.strings("secrets"),
    policy: fields.optionalString("policy"),
    ports: fields.list("ports").map(portForward),
    startedAt: fields.optionalDate("started_at"),
    createdAt: fields.date("created_at"),
  };
}

export function appExit(value: unknown): AppExit {
  const fields = Fields.of(value, "an app exit");

  return { exitCode: fields.int("code"), signal: fields.int("signal") || null, restarts: fields.int("restarts") };
}

export function policy(value: unknown): Policy {
  const fields = Fields.of(value, "a policy");
  const viewed = fields.optionalString("dns");

  return {
    name: fields.string("name"),
    rules: fields.list("rules").map(policyRule),
    holders: viewed === null ? null : fields.strings("holders"),
    dns: viewed === null ? null : fields.oneOf("dns", dnsModes),
  };
}

export function secretInfo(value: unknown): SecretInfo {
  const fields = Fields.of(value, "a secret");

  return {
    name: fields.string("name"),
    destinations: fields.strings("destinations"),
    placeholder: fields.string("placeholder"),
    updatedAt: fields.date("updated_at"),
  };
}

export function snapshot(value: unknown): Snapshot {
  const fields = Fields.of(value, "a snapshot");

  return {
    id: fields.string("id"),
    name: fields.optionalString("name"),
    source: fields.string("source"),
    sourceName: fields.optionalString("source_name"),
    image: fields.string("image"),
    digest: fields.string("digest"),
    provider: fields.string("provider"),
    diskMiB: fields.int("disk_mib"),
    memoryMiB: fields.int("memory_mib"),
    size: fields.int("size"),
    createdAt: fields.date("created_at"),
  };
}

export function port(value: unknown): Port {
  const fields = Fields.of(value, "a port");

  return {
    sandbox: fields.string("sandbox"),
    sandboxName: fields.optionalString("sandbox_name"),
    hostPort: fields.int("host_port"),
    guestPort: fields.int("guest_port"),
    public: fields.bool("public"),
    address: fields.string("address"),
    listening: fields.bool("listening"),
    error: fields.optionalString("error"),
    reachableOn: fields.list("reachable_on").map((on) => ({ interface: on.string("interface"), address: on.string("address") })),
  };
}

export function egressDecision(value: unknown): EgressDecision {
  const fields = Fields.of(value, "an egress decision");

  return {
    time: fields.date("time"),
    source: fields.oneOf("source", sources),
    verdict: fields.oneOf("verdict", actions),
    host: fields.optionalString("host"),
    port: fields.optionalInt("port", 0) || null,
    address: fields.optionalString("address"),
    rule: fields.string("rule"),
    ruleText: fields.optionalString("rule_text"),
    reason: fields.optionalString("reason"),
  };
}

export function version(value: unknown): Version {
  const fields = Fields.of(value, "a version");

  return { version: fields.string("version"), apiVersion: fields.string("api_version") };
}

export function capabilities(value: unknown): Capabilities {
  const fields = Fields.of(value, "the capabilities");

  return {
    create: fields.bool("create"),
    start: fields.bool("start"),
    stop: fields.bool("stop"),
    remove: fields.bool("remove"),
    pause: fields.bool("pause"),
    resume: fields.bool("resume"),
    fork: fields.bool("fork"),
    snapshot: fields.bool("snapshot"),
    port: fields.bool("port"),
  };
}

/** records reads a JSON array the daemon may answer as null when it is empty. */
export function records<T>(value: unknown, what: string, read: (record: unknown) => T): T[] {
  if (value === null || value === undefined) {
    return [];
  }
  if (!Array.isArray(value)) {
    throw new ProtocolError(`the daemon answered ${JSON.stringify(value)} as ${what}`);
  }

  return value.map(read);
}

function portForward(fields: Fields): PortForward {
  return { hostPort: fields.int("host_port"), guestPort: fields.int("guest_port"), public: fields.bool("public") };
}

function exitStatus(fields: Fields | null): ExitStatus | null {
  if (fields === null) {
    return null;
  }

  return { exitCode: fields.int("code"), signal: fields.int("signal") || null };
}

function restart(sandbox: Fields): RestartInfo | null {
  const fields = sandbox.optionalObject("restart");
  if (fields === null) {
    return null;
  }

  return {
    policy: fields.oneOf("policy", restartPolicies),
    retries: fields.optionalInt("retries", 0),
    backoff: fields.int("backoff"),
    count: fields.int("count"),
    lastAt: fields.optionalDate("last_at"),
    gaveUp: fields.bool("gave_up"),
    ended: fields.bool("ended"),
  };
}

/** policyRule spells a rule as the daemon's FormatRule does, less the action, so a set and its get compare equal. */
function policyRule(fields: Fields): PolicyRule {
  const destination = fields.object("destination");
  let rule = destination.string("value");
  if (destination.string("kind") === "domain-suffix") {
    rule = `suffix:${rule}`;
  }
  const protocol = fields.optionalString("protocol");
  if (protocol) {
    rule += ` ${protocol}`;
    const ports = fields.ints("ports");
    if (ports.length > 0) {
      rule += `:${ports.join(",")}`;
    }
  }

  return { action: fields.oneOf("action", actions), rule };
}
