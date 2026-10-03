# Egress

The host decides what a sandbox may reach, in netfilter, and the guest never does. A sandbox holds no
rules of its own, because gVisor's netstack forgets its iptables across a checkpoint and restore, and
whatever runs in the guest could rewrite them anyway. The host table is the policy of record. The
host writes it again, in one transaction, on every create, start, resume, fork, clone and policy
change.

## Without a policy

A sandbox created without `--policy` reaches the internet and nothing private. The host drops traffic
to its own networks (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), to link-local and cloud
metadata (`169.254.0.0/16`), to loopback (`127.0.0.0/8`), to carrier NAT (`100.64.0.0/10`) and to
every other sandbox. This floor holds under every policy too. No rule opens it, and `policy create`
refuses a rule that names `private`. The 403 and the egress log name this deny by its rule id,
`private`. In the same way they name `default` when no rule of a policy matches, and `missing` when
the policy does not exist. The proxy dials from the host itself, so under every policy it also
refuses a host that resolves to one of the host's own addresses, to `0.0.0.0/8`, or to multicast or
broadcast. It names that deny `local`.

## With a policy

```
shard policy create --allow api.openai.com --deny any locked
shard create --policy locked python:3.12 -- python agent.py
```

A policy is a name and an ordered list of rules. The first rule that matches a packet decides, and
the host drops a packet that matches none. A sandbox with a policy runs its own chain on the host,
keyed by the address its lease gave it. Every sandbox, with or without a policy, may send only from
its own address. The host pins the port to that address, in IP and in ARP, for as long as the lease
lasts.

A rule is `<destination> [tcp|udp[:<ports>]]`, and the form of the destination tells what it is:

| destination            | example                   | what it matches                                |
|------------------------|---------------------------|------------------------------------------------|
| an address or a prefix | `1.1.1.1`, `10.0.0.0/8`   | the one address, or the prefix                 |
| a name                 | `api.example.com`         | the addresses the name resolves to on the host |
| `any`                  | `any`                     | everything                                     |

A name may carry wildcard labels, and only the proxy matches them. `*.example.com` is every name
under the apex but not the apex itself. `www.*.com` swaps exactly one label, and `*` alone is every
host. A wildcard inside a label, such as `api*.example.com`, is refused. `suffix:example.com` is the
apex and every name under it, and only the proxy matches it too.

A name matches in any letter case, with or without a trailing dot. The store keeps it lowercase with
no trailing dot, so `policy show` prints `example.com` for a rule typed `ExAmPlE.com.`.

Ports are a comma-separated list of numbers and ranges, such as `tcp:22,8000-8100`. A rule with no
protocol matches every protocol, ping included. An address or prefix rule with no ports opens every
tcp and udp port to that destination, so name the ports when you want only some of them.

A name rule covers `tcp` to ports 80 and 443 only, and covers both when it names no port. A plain name
is enforced twice. The host table holds the addresses the name resolved to when the table was
written, and the proxy matches the name in the request. A wildcard and a suffix have no addresses to
resolve, so the host table skips them and only the proxy enforces them. The proxy speaks only HTTP
and TLS. A name on a raw port would need a guess at its addresses, so `policy create`
refuses it and says to use an address.

A secret grant does not open a destination, because only the policy decides egress. The policy must
allow a granted host like any other host. Under `deny any` a granted host is denied, and the value is
never put in. A sandbox with no policy keeps the default, which is the internet and nothing private.

## The proxy

A sandbox that holds a policy or a secret is fronted. The host turns its `tcp` 80 and 443 to the
egress proxy on the bridge gateway, on port 30080 for plain HTTP and 30443 for TLS. The proxy judges
every request by host name, with the same rules the host enforces. Fronting changes nothing else. The
host table still decides every other port, and a sandbox with neither a policy nor a secret is never
fronted.

The proxy is an HTTP proxy for 80 and 443 only. The host chain alone allows or drops every other port
and never brokers it, so a secret sent on such a port leaves as the placeholder. See
`docs/secrets.md`.

The proxy runs only in `shard daemon`, as its `proxy` task. Every verb goes over the daemon socket,
so only the process that runs the proxy ever creates, starts, forks or clones a fronted sandbox. No
path can front a sandbox and leave the DNAT pointing at nothing.

The proxy terminates TLS with its own CA. The CA is minted once per root under `${root}/proxy/`, with
the key at mode 0600. A fronted sandbox is built to trust it. The bundle merges the image's own roots
with the proxy CA at the path the image already reads, and points `SSL_CERT_FILE`,
`REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS` and `CURL_CA_BUNDLE` at that file. The last of these
overrides a bundle that the image names for curl, such as `/cacert.pem` in the official curl image,
because curl reads it first. A VM has no upper layer, so on a VM host the guest writes the same merged
bundle to the same path at first boot, and a fork's disk carries it. An image with no CA bundle is
refused, because a bundle that held only the proxy CA would make the guest trust nothing else. A
fronted create also refuses `--env` of any of those four names. The proxy resolves each name once, on
the host, and dials the address it judged. A TLS request without a server name is refused, and one
whose `Host` header disagrees with that name gets a 400.

A denied request gets a 403 with a one-line JSON body that names the host, the port, the rule and the
reason. When the upstream leg fails, the request gets a 502 with a fixed body. That body never holds
the error, because the error can quote the request after a secret value went into it. A secret value
goes into request headers only, so every body streams through unchanged (SHARD-337). One sandbox
holds at most 32 MiB at once of the bytes a value adds to a request, counted from the rewrite until
the request is forwarded. Past that limit a request gets a 503. The proxy reads the policy, the secret
and the sandbox records on every request, so a change takes effect on the next request. A connection
that is already open is not cut. The proxy logs one line per request, and never logs a header value,
a body or a secret.

A keep-alive connection that sends no next request is closed after 60 seconds. A sandbox holds at
most 1024 connections open to the proxy, over both ports. On Linux the host's input chain drops the
next one and writes it to the egress log as `limit`. On a VM host the stack does this (SHARD-350).
The proxy also counts connections by source and refuses any past 1024, with a daemon log line that
names the cap.

The rules for a fronted sandbox follow its record, as its chain does. A stopped sandbox keeps them,
`rm` removes them, and `start` writes them again.

`shard policy ls` lists the names. `shard policy rm` refuses while a sandbox record names the policy,
so remove the sandbox first. `shard policy show` prints `holders`, the sandboxes whose record names
the policy, and omits the field when no record does. `shard ls` prints a `POLICY` column, which
shows a dash when the sandbox holds no policy. Both read the records the way `rm` does, so they agree.

A policy that does not exist drops everything, so no flag overrides the refusal. The same rule holds
throughout: an error fails closed and never opens access.

## Attaching after the create

```
shard stop web
shard policy attach web locked
shard start web
shard policy detach web
```

`shard policy attach <id|name> <policy>` gives a sandbox that already exists the policy that a create
with `--policy` would have given it. The host enforces it from the next start. A sandbox holds one
policy, so an attach replaces the one it holds, and attaching the policy it already holds changes
nothing. `shard policy detach` leaves the sandbox with no policy, and does not touch its secrets or
their fronting.

Both verbs take only a created or stopped sandbox, for the same reason as the secret verbs. A running
guest holds its environment in its processes and a paused one holds it in its snapshot, so both are
refused with `stop it first`. A policy the host does not hold is refused, and the refusal writes
nothing.

When a sandbox held neither a policy nor a secret, the attach is what fronts it. So the attach first
plants the proxy CA in the writable layer, the same way a grant does. A detach leaves the CA in place.

The record and the host table always agree. The record is written first and the rules are applied
next. If the host refuses the rules, the record is put back as it was.

## What a policy implies

`shard inspect` prints `egress`, which lists what the host adds for the sandbox and then the policy's
rules. Each addition is marked `implied`:

- `dns`: a policy that names a domain or a suffix, or says `allow dns`, allows `udp` and `tcp` 53
  to shard's resolver on the bridge gateway and to nothing else, because a name is no use to a guest
  that cannot resolve it. An `allow any` that leaves port 53 open implies no rule, since it already
  reaches the resolver. A policy of only address rules opens no DNS, and a secret does not open it
  either. If the guest must resolve the host, name the host in the policy or say `allow dns`. When
  an explicit rule opened DNS, the implied rule reads `dns rule` instead of `dns`.

A sandbox with a policy resolves names only through shard's resolver. Its `resolv.conf` names the
gateway, and the host turns port 53 to anywhere else to the gateway, so a policy attached after the
create is enforced too. The resolver answers a question only when the policy allows the name, through
a name or suffix rule that matches it, through `allow any` or through `allow dns`. It then forwards
the question to the public nameservers. Every other name gets NXDOMAIN and stays unresolved, so a
lookup cannot carry a query out or an answer in past the policy. Every question is in the egress log,
with source `dns`. A sandbox with no policy keeps direct public DNS, whether or not a secret fronts
it.

One resolver process serves every policy sandbox on the host, so it bounds the share each sandbox
gets. Each source may have 16 questions or tcp connections in flight at once, under a limit of 256
for the whole host. A udp question past the bound is dropped, and the stub asks again. A tcp
connection past it is closed at accept. A sandbox that floods port 53 stalls only its own lookups.

A policy of only address rules needs care. `allow 203.0.113.7` gives the guest an address it can
reach but no way to resolve a name, so every tool that looks a name up first fails on the lookup. The
egress log shows this as a `dns` record that denies the name the tool asked for, and no request to
that host ever appears. A denied lookup with no request to that host is the sign of this case.
`shard policy create` prints a note about it when you store such a policy. It also prints one when
earlier denies cover every name that an allow opens, such as `deny any` before
`allow api.example.com`, because the resolver takes the first match. Add a name rule for the host, or
add `--allow dns`, and the implied `dns` rule opens with it.

A rule id is its position in the effective order. An edit that opens DNS on a policy that had none
puts two implied rules in front and moves every rule down by two. `shard inspect` and the `rule`
field of the egress log both name that position, so a log line written before the edit points at a
different rule after it. Any inserted rule has this effect, including `allow dns`, which looks
purely additive.

## Names are resolved on the host

A name rule compiles to the IPv4 addresses the name resolves to at apply time. The host resolves it
through the same public nameservers that shard's resolver forwards to, so a guest that answers its
own lookups changes nothing. As a result:

- A host whose addresses rotate can drift from the rule until the next apply. Store the policy again
  to apply it again.
- A name in a policy rule that does not resolve puts on hold only the sandboxes whose policy names
  it. The create or the start of such a sandbox fails. A running one keeps its last good chain while
  its policy is unchanged. Otherwise it gets a closed chain, where web traffic goes to the proxy, DNS
  goes to the resolver, and the rest is dropped. The last good chain lives in memory, so after a
  daemon restart the sandbox gets the closed chain. Every other sandbox carries on, and so do the
  daemon start, the proxy and the resolver. The daemon log names each held sandbox and the name.
- A held sandbox comes back on the first apply after the name resolves again. An apply is a create,
  start, rm or policy edit of any sandbox, or a daemon restart. Nothing retries on a timer.
- Policy create and update resolve every name first, even when no sandbox holds the policy, and
  refuse a name that does not resolve. A failed sandbox never runs, so the apply skips its policy.
- When many hosts share a CDN address, the host table allows that address for all of them. For 80
  and 443 the proxy closes this gap by matching the name in the request.

## The policy is IPv4, and IPv6 is dropped

Every match in the host table is `ip saddr` or `ip daddr`, which match IPv4 only, so no rule of a
policy can ever match an IPv6 packet. A guest gets an IPv4 address and an IPv4 default route, and
nothing sends router advertisements on the bridge, so a guest has no routable IPv6 address to send
from. The table does not rely on that, because it is only a matter of configuration. Every port drops
IPv6 as it arrives, and the `egress` chain drops it again for anything that reaches the forward path
another way.

Each drop is logged on rule `ipv6`, so it appears in `shard logs --egress` like any other decision.
For now a sandbox is IPv4 only. It gets one IPv4 address and no IPv6 route, and every IPv6 packet it
sends is dropped and logged. The design does not rule out IPv6, so support for it can come later.

## Where the policy is enforced, per substrate

| substrate | the table | what leaves the sandbox |
|---|---|---|
| gVisor, Sysbox, runc | host netfilter, one chain per sandbox with a policy | what the policy allows, with 80 and 443 through the proxy when fronted |
| Virtualization.framework | the judge of `pkg/netstack` inside the daemon, over the same compiled chains | the same: what the policy allows, with 80 and 443 through the proxy when fronted |

macOS has no host table. The guest's frames terminate in a userspace stack. The stack answers for
the gateway address, redirects a fronted sandbox's 80 and 443 onto the proxy the way the host chains
do, and asks a judge about every other TCP or UDP flow. The judge holds the same chains that the host
ruleset compiles from, and answers the way the `egress` chain does. The private floor drops first, a
sandbox without a policy reaches everything else, and a sandbox with a policy gets its rules in order
and then the default drop. The daemon dials a flow that the judge allows and splices it to the guest.
A refused flow gets no answer, just as a netfilter drop gives none. The stack writes it into the
sandbox's log as it refuses it, with `source` `host` and the `rule` the chain would have named. A
frame that is not a TCP or UDP flow carries `stack`, and so does a frame that reaches for a port the
daemon itself serves on an address other than the gateway. A sandbox without a policy or a secret is
not fronted, on a VM host the same as on Linux. Its 80 and 443 are judged like any other port and
reach the destination directly, so the TLS it sees is the destination's own (SHARD-294). Connection
tracking can keep a fronted sandbox's 80 or 443 off the proxy when the flow was first seen before the
sandbox was fronted, and such a flow carries `redirect`. Only the resolver on the gateway answers 53.
See `docs/provider-vz.md`.

## A policy change is immediate

Storing a policy again enforces it at once on every sandbox that holds it, running or paused. Nothing
restarts, because the whole table is replaced in one transaction. A connection that is already open
stays open until it ends, since the host accepts an established flow before it asks the policy. A new
connection is judged by the new rules.

## The decision log

Every fronted sandbox keeps a decision log. `shard logs --egress <id|name>` prints it as one JSON
record per line, oldest first. `shard logs -f --egress <id|name>` prints the same and then keeps
running, so a new record appears within about a second of the decision. The follow ends at Ctrl-C or
when the sandbox is removed, and it says on stderr which of the two happened. If the log renames a
file away before the follow has read it, the follow fails and says to follow again. A record names
the time, the source, the verdict, the host, the port, the address, the rule that decided, and that
rule's text. It never carries a header, a body or a secret value.

The log has three sources, and the daemon writes all of them into one file,
`${root}/sandboxes/<id>/egress.jsonl`:

- `proxy`: one record per request the proxy judged, allowed or denied. A decision that cannot be
  written fails closed, so the request is refused instead of going out unlogged.
- `dns`: one record per question the resolver judged, allowed or denied, with the name under
  `host` and no port or address. A question that cannot be written is refused the same way.
- `host`: one record per packet the host chains dropped, which covers everything the proxy and the
  resolver never saw. The chains log into the kernel ring buffer, and the daemon tails the ring into
  the file within a second of the drop. Reading the log never touches the ring.

The `rule` field holds the same id on both sides. It is the position of the rule in what
`shard inspect` prints as `egress`, or one of `private`, `default`, `local`, `ipv6`, `none`,
`missing`, `resolve` and, on a VM host, `stack`. A packet the guest sent to the host's own address
carries `local`. The host accepts the proxy ports and drops the rest, and logs that drop like any
other. A proxied request whose host resolves to such an address carries `local` too. An IPv6 packet
carries `ipv6`, and the record names it by the port it was dropped on instead of by its address.

The log has three limits:

- A drop that happens while the daemon is down is lost once the ring overwrites it. The daemon keeps the
  last kernel sequence it has passed in `${root}/egress.cursor`, and writes the ring's backlog when it
  starts. So a restart loses only what the ring itself overwrote in the meantime. The ring is
  host-wide and the cursor is per root, so a root with no cursor yet sets its cursor at the ring's
  end. The drops before its first start name no sandbox of that root, and its one summary line says
  so instead of blaming a sandbox that no longer exists. Each chain rule logs at 2 lines per second,
  with a burst of 10, so a probe storm cannot fill the ring.
- The log file is rotated at 8 MiB and one older file is kept, so a sandbox holds 16 MiB at most. The
  write that would pass 8 MiB renames the file first. `shard logs --egress` prints at most the newest
  10000 records, and says on stderr how many older ones it left out. A follow starts from the same
  newest 10000.
- A drop by another firewall on the host is not in the log. The daemon reads only the lines that
  shard's own chains write into the ring. A packet that `ufw`, `firewalld` or a rule of your own
  drops leaves no record, and the guest just times out. A rented box with `ufw` on is the common
  case. Its default `FORWARD` policy of `DROP` stops what a sandbox sends straight out, which is all
  of its traffic when it has no policy. An `INPUT` policy of `DROP` stops the resolver and the proxy
  on the bridge address. Let the bridge through with `ufw allow in on shard0` and
  `ufw route allow in on shard0`.
