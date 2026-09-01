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
| `any`                  | `any`                     | everything                                     |

A name may carry wildcard labels, matched by the proxy only: `*.example.com` is every name
under the apex and not the apex itself, `www.*.com` swaps exactly one label, and `*` alone is every
host. A wildcard inside a label, as `api*.example.com`, is refused.

Ports are a comma list of numbers and ranges, `tcp:22,8000-8100`. A rule with no protocol matches
every protocol, ping included.

A name rule is `tcp` to ports 80 and 443 only, and both when no port is named. Until the
egress proxy lands (SHARD-71) the host enforces the addresses the name resolves to when the table is
written. A name is then enforced through the proxy, which speaks HTTP and TLS and nothing else; on a raw port it would
be an address guess, so `policy create` refuses it and says to use an address. `suffix`
rules wait for the proxy (SHARD-71) and are refused until then.

`shard policy ls` lists the names, and `shard policy rm` refuses while a sandbox record names
the policy: `--force` overrides that, and the sandbox then reaches nothing, because a policy that does
not exist drops everything. That is the rule throughout: an error is a closed door, never an open one.

## What a policy implies

`shard inspect` prints `egress`, which is the policy's rules behind what the host adds for the
sandbox, each addition marked `implied`:

- **`secret NAME`**: a secret granted to the sandbox allows `tcp` 80 and 443 to every host it was
  granted to. The grant is the allow; the policy does not have to repeat it.
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
- A CDN address shared by many hosts is allowed for all of them. The proxy (SHARD-71) closes that
  by matching the name in the request.

## A policy change is immediate

Storing a policy again enforces it at once on every sandbox that holds it, running or paused. There
is no restart: the whole table is replaced in one transaction. A connection that is already open
stays open until it ends, since the host accepts an established flow before it asks the policy; a
new one is judged by the new rules.
