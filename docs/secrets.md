# Secrets

A sandbox never holds a secret value. It holds a placeholder, and the value is put into a request on
the host, on its way to the one destination the secret is granted to. Whatever runs in the sandbox,
a prompt-injected agent included, can read its environment, dump its memory and post every byte of it
anywhere it likes, and what it posts is the placeholder.

## The three parts

```
printf '%s' "$OPENAI_API_KEY" | shard secret set --to api.openai.com OPENAI_API_KEY
shard create --secret OPENAI_API_KEY python:3.12 -- python agent.py
```

**The store.** `shard secret set` reads the value from stdin, so it lands in no shell history and no
process listing, and writes it to `<root>/secrets/<NAME>`, mode 0600 under a directory of mode 0700.
That file is the only place the value is written. `shard secret ls` prints names, destinations and
placeholders, never a value. `shard secret rm` refuses while a sandbox record names the secret, and
`--force` overrides that. The name is the environment variable the guest reads, so it is shaped like
one: uppercase letters, digits and `_`.

**The grant.** A secret is granted to a destination, never to a sandbox alone. `--to` names the
hosts the value may go to, and a request to any other host never carries it. `shard create --secret
NAME` hands the guest the placeholder as `$NAME` and records the grant in the sandbox record, which
`shard inspect` prints as `secrets`. A fork and a clone carry the grant of their source, because the
copied bundle already hands the guest the placeholder.

**The substitution.** The placeholder defaults to `mock-NAME` and `--mock-value` sets another, for
an SDK that checks the shape of a key before it sends it. On the way out, the egress proxy replaces
the placeholder with the value, in the URL, the headers and the body, and only when the request is
bound for a granted destination. A request to any other host carries the placeholder as it is.

**The proxy.** A sandbox that holds a secret sends its port 80 and 443 through the egress proxy: the
host turns those two ports to it, and nothing else on the host answers them, so a sandbox with no
proxy reaches no web host rather than every one. The proxy terminates TLS with a CA of its own, minted
once per root under `ca/`, and the guest trusts it: the bundle appends it to the image's certificate
bundle and sets `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS` unless the image set
them. The first verb that needs the proxy starts it, detached, and `shard proxy` runs one in the
foreground for an operator who wants to watch it. Its log is `proxy/log` under the root.

What that means for a client:

- HTTP/1.1 only. A client that insists on HTTP/2 does not connect.
- A TLS connection must name its host (SNI), and a request whose `Host` names another is refused.
- A raw TCP or UDP port, 22 or 5432 say, is not the proxy's and is never substituted: it goes by the
  policy alone, and the placeholder is what reaches it.
- The body is rewritten up to 8 MiB. A bigger one goes through as it is, with the placeholder.
- The proxy matches the placeholder as bytes. A client that encodes it, base64 in a basic-auth header
  say, sends the placeholder.
- A reflection is not a leak: an allowed host that echoes the request shows the guest the value in the
  response. Scope the key at the provider.

## What it stops, and what it does not

It stops theft: the value cannot leave through the sandbox because the sandbox never had it.

It does not stop misuse. A sandbox that may talk to `api.openai.com` with the key may make any call
that key allows, and a compromised agent can run up a bill or read what the key can read. Scope the
key at the provider; shard only keeps it from leaving.

## Rotation

`shard secret set` again with the same name replaces the value, and keeps the grant and the
placeholder unless `--to` or `--mock-value` say otherwise. A new placeholder is refused while a
sandbox holds the old one, because that sandbox would never be matched again. Nothing caches the
value: the proxy reads the store per request, so a live sandbox uses the new value on its next
request and never learns that anything changed.
