# Egress

What a sandbox may reach is decided on the host, in netfilter, and never in the guest. A sandbox
holds no rules of its own: gVisor's netstack forgets its iptables across a checkpoint and restore,
and whatever runs in the guest could rewrite them anyway. The host table is the policy of record,
and it is written again, in one transaction, on every create, start, resume, fork, clone and policy
change.

## Without a policy

A sandbox created without `--policy` reaches the internet and nothing private: the host's own
networks (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local and cloud metadata
(`169.254.0.0/16`), loopback (`127.0.0.0/8`) and carrier NAT (`100.64.0.0/10`) are dropped, and so
is every other sandbox. That floor holds under every policy too: no rule opens it, and
`policy create` refuses a rule that names `private`.

## With a policy

```
shard policy create --allow api.openai.com --deny any locked
shard create --policy locked python:3.12 -- python agent.py
```

A policy is a name and an ordered list of rules. The first rule that matches a packet decides, and a
packet that matches none is dropped. A sandbox with a policy runs its own chain on the host, keyed by
the address its lease gave it. Every sandbox, with a policy or without, may send from its own address
only: the host pins the port to the address, in IP and in ARP, for as long as the lease lasts.

A rule is `<destination> [tcp|udp[:<ports>]]`, and the destination's shape says what it is:

| destination            | example                   | what it matches                                |
|------------------------|---------------------------|------------------------------------------------|
| an address or a prefix | `1.1.1.1`, `10.0.0.0/8`   | the one address, or the prefix                 |
| a name                 | `api.example.com`         | the addresses the name resolves to on the host |
| `suffix:<name>`        | `suffix:example.com`      | the name and every name under it, at the proxy |
| `any`                  | `any`                     | everything                                     |

A name may carry wildcard labels, matched by the proxy only: `*.example.com` is every name
under the apex and not the apex itself, `www.*.com` swaps exactly one label, and `*` alone is every
host. A wildcard inside a label, as `api*.example.com`, is refused.

Ports are a comma list of numbers and ranges, `tcp:22,8000-8100`. A rule with no protocol matches
every protocol, ping included.

A name rule is `tcp` to ports 80 and 443 only, and both when no port is named. A name is
enforced through the egress proxy, which speaks HTTP and TLS and nothing else; on a raw port it would
be an address guess, so `policy create` refuses it and says to use an address. A `suffix`
rule lives at the proxy alone: it compiles to nothing on the host, and matches the name a request
carries.

## The proxy fronts a sandbox with a policy or a secret

A sandbox that holds a policy or a secret is *fronted*: the host turns its port 80 and 443 to the
egress proxy on the gateway, and the proxy judges each request by the name it carries, with the same
rules in the same order the host holds. So a web request meets the policy twice, first by name at
the proxy and then by address on the host for everything else. What the host cannot see, a name
behind a shared CDN address say, the proxy can. A fronted sandbox with no proxy reaches no web host:
the first verb that needs one starts it, and refuses the sandbox if it does not come up. When
`shard serve` runs it owns the proxy: it serves one itself, restarts it when it crashes, and takes
over from a one-shot proxy when that one dies. Without the daemon a dead proxy stays dead
until the next `create`, `start`, `resume`, `fork` or `clone` starts another.

A rule at the proxy reads as it does on the host, with one addition: a host a grant implies, and a
sandbox with no policy at all, may not reach a private address by name. Written into the policy by the
operator, the same name rule may.

`shard policy ls` lists the names, `shard ls` shows the policy each sandbox holds in its `POLICY`
column, and `shard policy show` prints `holders`, the sandboxes whose record names the policy.
`shard policy rm` refuses while a sandbox record names
the policy: remove the sandbox first. A policy that does not exist drops everything, so no flag
overrides the refusal. That is the rule throughout: an error is a closed door, never an open one.

## What a policy implies

`shard inspect` prints `egress`, which is the policy's rules behind what the host adds for the
sandbox, each addition marked `implied`:

- **`secret NAME`**: a secret granted to the sandbox allows `tcp` 80 and 443 to every host it was
  granted to. The grant is the allow; the policy does not have to repeat it, and it cannot take it
  back: the implied rules come before the policy's own, so a `deny` of a granted host does not match.
  Remove the grant to close the host.
- **`dns`**: a policy that names a domain, or a sandbox that holds a secret, allows `udp` and `tcp`
  53 to the sandbox nameservers. A name is no use to a guest that cannot resolve it. A policy of only
  address and `any` rules opens no DNS.

## Names are resolved on the host

A name rule compiles to the IPv4 addresses the name resolves to at apply time, through the same
nameservers the guest uses, so a guest that answers its own lookups changes nothing. What that means:

- A host whose addresses rotate can drift from the rule until the next apply. Store the policy again
  to apply it again.
- A name in a policy rule that does not resolve fails the apply, and with it the create, the start
  or the policy command that asked for it. The host keeps the rules it had.
- A name a grant implies is not in the policy, so one that does not resolve closes that host and
  nothing else: the sandbox comes up, and it cannot reach that host until the next apply.
- A CDN address shared by many hosts is allowed for all of them on a raw port. On 80 and 443 the proxy
  closes that by matching the name in the request.

## Every decision is logged

`shard logs --egress ID` prints every decision on the sandbox's traffic, one JSON line each, oldest
first:

```
{"time":"...","sandbox":"...","source":"proxy","verdict":"deny","protocol":"tcp","destination":"example.org:443","address":"93.184.216.34","rule":"default","reason":"policy locked has no rule for example.org:443"}
{"time":"...","sandbox":"...","source":"host","verdict":"deny","protocol":"icmp","destination":"8.8.8.8","address":"8.8.8.8","rule":"1","rule_text":"deny any"}
```

`rule` is the index into the rules `shard inspect` prints under `egress`, and `rule_text` spells that
rule as the policy reads today, so a line older than the last policy change may name a rule that has
moved. When no rule decided, `rule` says what did: `default` (nothing matched, so dropped), `private`
(the floor under every policy), `none` (no policy, so allowed), `missing` (the policy is gone) or
`resolve` (the name did not resolve). Two sources feed it:

- **`proxy`**: every web request the proxy judged, allowed or denied, written to `egress.jsonl` in
  the sandbox directory as it happens.
- **`host`**: every packet the host dropped, read from the kernel log, where the netfilter rules write
  every unanswered packet (a retried SYN or probe counts again), at most 2 a second per rule. The
  host logs no accept: an accept is every packet of a flow, and the web is already in the proxy's
  lines.

The kernel log is one short ring for the whole host, shared by every sandbox, so old host lines fall
off it. The kernel clock drifts from wall time on a long-up host, so `logs --egress` writes a mark
into the ring first and dates every host line against it; a host line still sorts some milliseconds
away from a proxy line of the same moment. While `shard serve` runs it rotates an oversized proxy
file and keeps one old generation, which `logs --egress` still reads; without the daemon the file
grows unbounded.

## A policy change is immediate

Storing a policy again enforces it at once on every sandbox that holds it, running, paused or
stopped. There is no restart: the whole table is replaced in one transaction. A stopped sandbox keeps
its address, so it keeps its chain: a start finds the rules in place before the guest runs. A
connection that is already open stays open until it ends, since the host accepts an established flow
before it asks the policy; a new one is judged by the new rules.
