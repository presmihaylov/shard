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
is every other sandbox. That floor holds under every policy too: no rule opens it.

## With a policy

```
shard policy create --allow domain:api.openai.com --deny group:any locked
shard create --policy locked python:3.12 -- python agent.py
```

A policy is a name and an ordered list of rules. The first rule that matches a packet decides, and a
packet that matches none is dropped. A sandbox with a policy runs its own chain on the host, keyed by
the address its lease gave it. Every sandbox, with a policy or without, may send from its own address
only: the host pins the port to the address, in IP and in ARP, for as long as the lease lasts.

A rule is `<kind>:<value> [tcp|udp[:<ports>]]`:

| kind     | value                          | what it matches                                |
|----------|--------------------------------|------------------------------------------------|
| `cidr`   | `10.0.0.0/8`, `1.1.1.1`        | the prefix, or the one address                 |
| `domain` | `api.example.com`              | the addresses the name resolves to on the host |
| `group`  | `private`, `any`               | the private ranges above, or everything        |

Ports are a comma list of numbers and ranges, `tcp:22,8000-8100`. A rule with no protocol matches
every protocol, ping included.

A `domain` rule is `tcp` to ports 80 and 443 only, and both when no port is named. A name is
enforced through the egress proxy, which speaks HTTP and TLS and nothing else; on a raw port it would
be an address guess, so `policy create` refuses it and says to use a `cidr` rule. `domain-suffix`
rules wait for the proxy (SHARD-71) and are refused until then.

`shard policy apply -f FILE` stores the same thing from JSON, in the shape `shard policy show`
prints. `shard policy ls` lists the names, and `shard policy rm` refuses while a sandbox record names
the policy: `--force` overrides that, and the sandbox then reaches nothing, because a policy that does
not exist drops everything. That is the rule throughout: an error is a closed door, never an open one.

## What a policy implies

`shard inspect` prints `egress`, which is the policy's rules behind what the host adds for the
sandbox, each addition marked `implied`:

- **`secret NAME`**: a secret granted to the sandbox allows `tcp` 80 and 443 to every host it was
  granted to. The grant is the allow; the policy does not have to repeat it.
- **`dns`**: a policy that names a domain, or a sandbox that holds a secret, allows `udp` and `tcp`
  53 to the sandbox nameservers. A name is no use to a guest that cannot resolve it. A policy of only
  `cidr` and `group` rules opens no DNS.

## Names are resolved on the host

A `domain` rule compiles to the IPv4 addresses the name resolves to at apply time, through the same
nameservers the guest uses, so a guest that answers its own lookups changes nothing. What that means:

- A host whose addresses rotate can drift from the rule until the next apply. Store the policy again
  to apply it again.
- A name in a policy rule that does not resolve fails the apply, and with it the create, the start
  or the policy command that asked for it. The host keeps the rules it had.
- A name a grant implies is not in the policy, so one that does not resolve closes that host and
  nothing else: the sandbox comes up, and it cannot reach that host until the next apply.
- A CDN address shared by many hosts is allowed for all of them. The proxy (SHARD-71) closes that
  by matching the name in the request.

## A policy change is immediate

Storing a policy again enforces it at once on every sandbox that holds it, running or paused. There
is no restart and no window with the old rules: the whole table is replaced in one transaction.
