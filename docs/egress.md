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
host. A wildcard inside a label, as `api*.example.com`, is refused. `suffix:example.com` is the
apex and every name under it, and is matched by the proxy only too.

Ports are a comma list of numbers and ranges, `tcp:22,8000-8100`. A rule with no protocol matches
every protocol, ping included.

A name rule is `tcp` to ports 80 and 443 only, and both when no port is named. A plain name is
enforced twice: the host table holds the addresses it resolved to when the table was written, and
the proxy matches the name in the request. A wildcard and a suffix have no addresses to resolve, so
the host table skips them and the proxy alone enforces them. The proxy speaks HTTP and TLS and
nothing else; a name on a raw port would be an address guess, so `policy create` refuses it and
says to use an address.

## The proxy

A sandbox that holds a policy or a secret is fronted: the host turns its `tcp` 80 and 443 to the
egress proxy on the bridge gateway, port 30080 for plain HTTP and 30443 for TLS, and the proxy
judges every request by host name with the same rules the host enforces. Nothing else changes: the
host table still decides every other port, and a sandbox with neither is never fronted.

The proxy runs in `shard daemon` only, as its `proxy` task. A verb that fronts a sandbox, so a
`create` with `--policy` or `--secret` and a `start`, `fork` or `clone` of a sandbox whose record
holds one, checks that the daemon is up first and refuses when it is not:

```
shard daemon is not running; start it (systemctl start shard) before fronting a sandbox
```

An unfronted sandbox is never created in its place, and a verb that fronts nothing needs no daemon.

The proxy terminates TLS with its own CA, minted once per root under `${root}/proxy/` with the key
at mode 0600. A fronted sandbox is built to trust it: the bundle merges the image's own roots with
the proxy CA at the path the image already reads, and points `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`
and `NODE_EXTRA_CA_CERTS` at it. An image with no CA bundle is refused, since a bundle holding the
proxy CA alone would make the guest trust nothing else, and `--env` of any of those three names is
refused on a fronted create. The proxy resolves each name once, on the host, and dials the address
it judged. A TLS request without a server name is refused, and one whose `Host` header disagrees
with it gets a 400.

A denied request gets a 403 with a one-line JSON body naming the host, the port, the rule and the
reason. The proxy rewrites a body of up to 8 MiB; a longer one streams through unchanged. It reads
the policy, the secret and the sandbox records on every request, so a change lands on the next
request; a connection that is already open is not cut. The proxy logs one line per request and
never a header value, a body or a secret.

The rules for a fronted sandbox follow its record like its chain: a stopped sandbox keeps them, `rm`
removes them and `start` writes them again.

`shard policy ls` lists the names, and `shard policy rm` refuses while a sandbox record names
the policy: remove the sandbox first. A policy that does not exist drops everything, so no flag
overrides the refusal. That is the rule throughout: an error is a closed door, never an open one.

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
- A CDN address shared by many hosts is allowed for all of them on the host table. The proxy closes
  that for 80 and 443 by matching the name in the request.

## A policy change is immediate

Storing a policy again enforces it at once on every sandbox that holds it, running or paused. There
is no restart: the whole table is replaced in one transaction. A connection that is already open
stays open until it ends, since the host accepts an established flow before it asks the policy; a
new one is judged by the new rules.
