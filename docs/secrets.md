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
copied filesystem already holds the placeholder.

**The substitution.** The placeholder defaults to `mock-NAME` and `--mock-value` sets another, for
an SDK that checks the shape of a key before it sends it. On the way out, the egress proxy replaces
the placeholder with the value, in the URL, the headers and the body, and only when the request is
bound for a granted destination. That is the substitution SHARD-71 ships; before it, the placeholder
reaches the wire as it is.

## What it stops, and what it does not

It stops theft: the value cannot leave through the sandbox because the sandbox never had it.

It does not stop misuse. A sandbox that may talk to `api.openai.com` with the key may make any call
that key allows, and a compromised agent can run up a bill or read what the key can read. Scope the
key at the provider; shard only keeps it from leaving.

## Rotation

`shard secret set` again with the same name replaces the value. Nothing caches it: the proxy reads
the store per request, so a live sandbox uses the new value on its next request and never learns
that anything changed.
