# Secrets

A sandbox never holds a secret value. It holds a placeholder, and the value is put into a header of an
HTTPS request on the host, on its way to the one destination the secret is granted to. Whatever runs in the
sandbox, a prompt-injected agent included, can read its environment, dump its memory and post every
byte of it anywhere it likes, and what it posts is the placeholder. That holds as long as the granted
host never sends the value back: see the caution under the grant.

## The three parts

```
printf '%s' "$OPENAI_API_KEY" | shard secret set --to api.openai.com OPENAI_API_KEY
shard create --secret OPENAI_API_KEY python:3.12 -- python agent.py
```

**The store.** `shard secret set` writes the value to `<root>/secrets/<NAME>`, mode 0600 under a
directory of mode 0700. That file is the only place the value is written. `shard secret ls` prints names, destinations and
placeholders, never a value. `shard secret rm` refuses while a sandbox record names the secret, and
`--force` overrides that. The name is the environment variable the guest reads, so it is shaped like
one: uppercase letters, digits and `_`.

**The grant.** A secret is granted to a destination, never to a sandbox alone. `--to` names the
hosts the value may go to, and a request to any other host never carries it. `shard create --secret
NAME` hands the guest the placeholder as `$NAME` and records the grant in the sandbox record, which
`shard inspect` prints as `secrets`. A fork and a clone carry the grant of their source, because the
copied bundle already hands the guest the placeholder.

**Caution: grant only to hosts that never echo the credential.** The proxy puts the value into the
request headers alone, never the URL or the body, and reads nothing out of the response. A granted
host that reflects a header it received, an echo endpoint or a debug page that prints its request
headers, still returns the raw value in the body, and the guest reads it there. Stripping the value from every response is not practical, so the grant is the
control: name only hosts that consume the credential and never return it.

A grant does not open the host and does not close anything. The sandbox's policy decides what it may
reach; the grant decides only where the value may be put in. A sandbox with a policy needs an allow
for the granted host in that policy.

## Granting after the create

```
shard stop web
shard secret grant web OPENAI_API_KEY
shard start web
shard secret ungrant web OPENAI_API_KEY
```

`shard secret grant <id|name> <NAME>` hands a sandbox that already exists the same placeholder a
create with `--secret` would have given it: the placeholder goes into the bundle environment, the
proxy CA is planted in the writable layer, and the grant goes into the record. `shard secret ungrant`
takes the placeholder and the grant back, and leaves the CA in place. A VM sandbox has no bundle, so
there both edits land in the run message its record holds, which the guest reads at its next start.

Both verbs take a created or stopped sandbox only. A running guest holds its environment in its
processes and a paused one holds it in its snapshot, so both are refused with `stop it first`. Both
verbs are safe to run again: a grant the record already names changes nothing.

A grant is refused when the guest environment already holds that name, and a refused grant writes
nothing at all. A secret named for a trust variable, such as `SSL_CERT_FILE` or `CURL_CA_BUNDLE`, is
refused at create and at a grant, since the proxy points those at the trust store. `shard secret rm`
refuses while any sandbox holds a grant and names the holders: ungrant it first, remove those
sandboxes, or pass `--force`.

**The substitution.** The placeholder is `mock-NAME` by default. A sandbox that holds a secret is
fronted: the host turns its HTTP on 80 and 443 to the egress proxy, which is where the value goes
in. See `docs/egress.md` for what fronting means. On the way out, the proxy replaces the placeholder with
the value in the request headers only, and only when the request goes over TLS to a granted
destination. A placeholder in the path, the query or the body goes upstream as it is: the guest picks
those fields, and an upstream that quotes one back in an error, a 404 that names the model it was
asked for, would hand the guest the value (SHARD-337). Put the key in a header, where every SDK puts it. A request to any other host carries the placeholder as it is, so a guest that posts its
environment to an attacker posts the placeholder. Plain HTTP on 80 never gets the value, not even to a
granted host, because it crosses the network in cleartext: the proxy forwards it with the placeholder
unchanged, so send the credential over `https://`. HTTP Basic auth is decoded, substituted and
re-encoded, so `https://api:mock-KEY@host` works. Any other encoding or signing of the key is not
substituted; the proxy finds the placeholder only where it appears verbatim in a header value or inside a Basic header.
A hop-by-hop header, `Connection`, `Upgrade`, `Keep-Alive` and any header `Connection` names, keeps
the placeholder: it belongs to the connection, not the upstream, and the proxy can quote one back.
Brokering covers HTTPS on port 443 today. A credential sent on any other port or protocol, a
database password on 5432 or SMTP on 587, leaves as the placeholder, never the value; the
policy still decides whether the connection is allowed at all.

**The placeholder.** An SDK that checks the shape of a key before it sends it never sends
`mock-NAME`, so `--placeholder` gives the guest a string of the right shape:

```
shard secret set --to api.stripe.com --placeholder sk_test_placeholder01 STRIPE_KEY
```

A chosen placeholder is letters, digits, `_`, `-` and `.`, so no URL, JSON or base64 encoder ever
changes it on the way out. It is refused when it is inside the value, when it is shorter than 8
characters, when it holds anything outside that set, or when another secret already owns it as its own
placeholder or as its default. The default `mock-NAME` is exempt from all but the first, so a short
name still gets one. Only a placeholder this call names is checked for shape, so a rotation is never
blocked by the one the record carries forward. Changing the placeholder of a secret a sandbox holds
is refused: that guest already holds the old one, so ungrant it first. `shard secret ls` prints the placeholder.

**The value.** `shard secret set` takes it three ways. It reads stdin when the value is `-` or when
stdin is a pipe, which is the way to use in a script, because the value then lands in no shell
history and no process listing. With a terminal and no value it prompts with the echo off. A value
given on the command line is stored, and `set` prints one caution to stderr: `ps` showed the value
while the command ran. A `set` the store refused prints the same caution, because the value was on
the command line either way and the operator is about to type it again.

## Clients the proxy certificate does not reach

The proxy CA is planted at the path the image already reads, and `SSL_CERT_FILE`,
`REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS` and `CURL_CA_BUNDLE` point at it. OpenSSL and everything
on it reads it: curl, Python, Ruby, PHP, Go and .NET on Linux. So do Python `requests` and `httpx`,
and Node, Deno and Bun. Three kinds of client do not.

**Java** trusts its own keystore. Import the CA in the image with `keytool -importcert`, or point
`-Djavax.net.ssl.trustStore` at a store that holds it. The CA is the file `SSL_CERT_FILE` names.

**Rust built with `rustls` and `webpki-roots`** compiles its roots in. Build with
`rustls-native-certs` instead, which reads the same file.

**Any client that pins a certificate or a public key** rejects the proxy by design and cannot be
fronted. The call fails closed with a certificate error, so nothing leaks: the request never goes
out and the placeholder is never substituted.

## What it stops, and what it does not

It keeps the value out of the sandbox: the guest can read its environment, dump its memory and search
its disk, and find the placeholder. The value goes into request headers alone, so the guest cannot
steer it into a field the upstream quotes back. The one way back in is a granted host that echoes a
header it received, which is why a grant names only hosts that never do. On the `runc` provider the guarantee is the proxy's alone: nothing isolates the guest
from the host, root in the guest is root on the host, and an escape reads the store as the host does.

It does not stop misuse. A sandbox that may talk to `api.openai.com` with the key may make any call
that key allows, and a compromised agent can run up a bill or read what the key can read. Scope the
key at the provider; shard only keeps it from leaving.

## Rotation

`shard secret set` again with the same name replaces the value, and keeps the grant and the
placeholder unless `--to` or `--placeholder` say otherwise. Nothing caches the value: the proxy
reads the store per request, so a live sandbox uses the new value on its next request and never
learns that anything changed.

A rotation that also moves the placeholder is refused while any sandbox holds a grant on the secret,
because that sandbox holds the old placeholder and would never be matched again: ungrant it there
first. It is refused too when the sandbox records cannot be read, since nothing can then say the
secret is free.

## A grant may name a wildcard

`secret set --to '*.github.com' NAME` grants the value to every host under the apex. The `*` is the
leftmost label alone and must stand over a registrable domain, so `*.github.com` and `*.openai.com` are
taken but `*.*`, `*.com`, `api.github.*`, a bare `*` and a public suffix like `*.co.uk` or `*.github.io`
are refused: the value must bind to a domain the owner controls. The public suffix list is bundled with
the `golang.org/x/net` version and updates on a `go.mod` bump.

A secret bound before this rule to a broad or public-suffix destination stops substituting on that
destination until you re-set it with a valid one. The store drops the unsafe destination on read, so the
value is never put into a request for a host the owner does not control.
