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

**The substitution.** The placeholder is `mock-NAME` by default. A sandbox that holds a secret is
fronted: the host turns its web traffic to the egress proxy, which is where the value goes in. See
`docs/egress.md` for what fronting means. On the way out, the proxy replaces the placeholder with
the value, in the URL, the headers and the body, and only when the request is bound for a granted
destination. A request to any other host carries the placeholder as it is, so a guest that posts its
environment to an attacker posts the placeholder. A body past 8 MiB streams through untouched: put
the key in a header, where every SDK puts it. A credential the client encodes before it sends it,
HTTP Basic auth included, is not substituted; the proxy finds the placeholder only where it appears
verbatim.

**The placeholder.** An SDK that checks the shape of a key before it sends it never sends
`mock-NAME`, so `--placeholder` gives the guest a string of the right shape:

```
shard secret set --to api.stripe.com --placeholder sk_test_placeholder01 STRIPE_KEY
```

A chosen placeholder is refused when it is inside the value, when it is shorter than 8 characters,
when it holds whitespace or a control character, or when another secret already owns it as its own
placeholder or as its default. The default `mock-NAME` is exempt from all but the first, so a short
name still gets one. Changing the placeholder of a secret a sandbox holds is refused: that guest
already holds the old one, so ungrant it first. `shard secret ls` prints the placeholder.

**The value.** `shard secret set` takes it three ways. It reads stdin when the value is `-` or when
stdin is a pipe, which is the way to use in a script, because the value then lands in no shell
history and no process listing. With a terminal and no value it prompts with the echo off. A value
given on the command line is stored, and `set` prints one caution to stderr: `ps` showed the value
while the command ran.

## What it stops, and what it does not

It stops theft: the value cannot leave through the sandbox because the sandbox never had it. The
guest can read its environment, dump its memory and search its disk, and find the placeholder.

It does not stop misuse. A sandbox that may talk to `api.openai.com` with the key may make any call
that key allows, and a compromised agent can run up a bill or read what the key can read. Scope the
key at the provider; shard only keeps it from leaving.

## Rotation

`shard secret set` again with the same name replaces the value, and keeps the grant and the
placeholder unless `--to` or `--placeholder` say otherwise. Nothing caches the value: the proxy
reads the store per request, so a live sandbox uses the new value on its next request and never
learns that anything changed.

A rotation that also moves the placeholder is refused while any sandbox holds a grant on the secret,
because that sandbox holds the old placeholder and would never be matched again: ungrant those
sandboxes first. It is refused too when the sandbox records cannot be read, since nothing can then
say the secret is free.

## A grant may name a wildcard

`secret set --to '*.github.com' NAME` grants the value to every host under the apex. A wildcard
label follows the same shape as a policy name rule, and a grant of `*` alone is refused: the
value must be bound to a name.
