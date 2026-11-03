// The records the daemon answers about sandboxes, processes, ports, policies, secrets and snapshots, read into camelCase.
import { Fields } from "./decode.js";
import { ProtocolError } from "./errors.js";

export type SandboxState = "pending" | "created" | "running" | "paused" | "unresponsive" | "stopped" | "failed";

const states: readonly SandboxState[] = ["pending", "created", "running", "paused", "unresponsive", "stopped", "failed"];

export type RestartPolicy = "no" | "on-failure" | "always" | "unless-stopped";

const restartPolicies: readonly RestartPolicy[] = ["no", "on-failure", "always", "unless-stopped"];

/** ProcessState is where a process stands; exited, killed, gave-up and stopped are the states its policy ended it in. */
export type ProcessState = "running" | "restarting" | "exited" | "killed" | "gave-up" | "stopped";

const processStates: readonly ProcessState[] = ["running", "restarting", "exited", "killed", "gave-up", "stopped"];

/** ExitStatus is how a process ended: its exit code, and the signal that ended it or null. */
export interface ExitStatus {
  exitCode: number;
  signal: number | null;
}

export interface Resources {
  /** 0 is no bound. */
  memoryMiB: number;
  vcpus: number;
  diskMiB: number;
  /** The swap file on the disk, which diskMiB counts; 0 is none. */
  swapMiB: number;
}

/** Restart says when shard-init starts a process again; backoff is the first wait in seconds, which doubles up to 60. */
export interface Restart {
  policy: RestartPolicy;
  /** The starts again in a row on-failure allows before it gives up; 0 or absent is no cap. */
  retries?: number;
  backoff?: number;
}

/** OOMInfo is what the host's memory kills did to a sandbox, which the daemon starts again after each one. */
export interface OOMInfo {
  /** Every time the host ended the sandbox for its memory. */
  kills: number;
  killedAt: Date;
  /** When the daemon starts the sandbox again; null once a start ran or a stop called it off. */
  restartAt: Date | null;
}

/** ProcessStatus is what shard-init last said of a process. */
export interface ProcessStatus {
  state: ProcessState;
  /** The starts again since the process was run or its sandbox started. */
  restarts: number;
  /** null before the first exit. */
  exit: ExitStatus | null;
  startedAt: Date | null;
}

/** ProcessInfo is one named process a run started in a sandbox. */
export interface ProcessInfo {
  name: string;
  command: string[];
  env: string[];
  workdir: string | null;
  user: string | null;
  restart: Restart;
  /** A kill keeps an unless-stopped process down on the next start; a run of the same name clears it. */
  killed: boolean;
  status: ProcessStatus;
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
  /** null for a sandbox the host never ended for its memory. */
  oom: OOMInfo | null;
  failedReason: string | null;
  resources: Resources;
  /** The processes a run started, in the order they were first run. */
  processes: ProcessInfo[];
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

/** Capabilities say which of the nine lifecycle verbs the daemon's provider supports, and whether it gives a sandbox swap; snapshot is the creation of a filesystem snapshot, and port the forward of a host port. */
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
  swap: boolean;
}

export function sandboxInfo(value: unknown): SandboxInfo {
  const fields = Fields.of(value, "a sandbox");
  const resources = fields.object("resources");

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
    oom: oom(fields.optionalObject("oom")),
    failedReason: fields.optionalString("failed_reason"),
    resources: { memoryMiB: resources.int("memory_mib"), vcpus: resources.int("vcpus"), diskMiB: resources.int("disk_mib"), swapMiB: resources.int("swap_mib") },
    processes: fields.list("processes").map(processOf),
    secrets: fields.strings("secrets"),
    policy: fields.optionalString("policy"),
    ports: fields.list("ports").map(portForward),
    startedAt: fields.optionalDate("started_at"),
    createdAt: fields.date("created_at"),
  };
}

export function processInfo(value: unknown): ProcessInfo {
  return processOf(Fields.of(value, "a process"));
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
    swap: fields.bool("swap"),
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

function oom(fields: Fields | null): OOMInfo | null {
  if (fields === null) {
    return null;
  }

  return { kills: fields.int("kills"), killedAt: fields.date("killed_at"), restartAt: fields.optionalDate("restart_at") };
}

function processOf(fields: Fields): ProcessInfo {
  const restart = fields.object("restart");
  const status = fields.object("status");

  return {
    name: fields.string("name"),
    command: fields.strings("command"),
    env: fields.strings("env"),
    workdir: fields.optionalString("workdir"),
    user: fields.optionalString("user"),
    restart: { policy: restart.oneOf("policy", restartPolicies), retries: restart.optionalInt("retries", 0), backoff: restart.optionalInt("backoff", 0) },
    killed: fields.bool("killed"),
    status: {
      state: status.oneOf("state", processStates),
      restarts: status.int("restarts"),
      exit: exitStatus(status.optionalObject("exit")),
      startedAt: status.optionalDate("started_at"),
    },
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
