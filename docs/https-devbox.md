# The HTTPS endpoint on a devbox

devbox-shard2 can serve a real `shard serve` over HTTPS, so the SDKs and the remote client have
something to test against: `https://<the box's address with dashes>.sslip.io`, which for
devbox-shard2 is `https://2-28-127-132.sslip.io`. The `shard-https` role in nairi-infra deploys it,
and `/usr/local/sbin/shard-https` on the box drives it.

## The shape

```
client --https:443--> Caddy --http--> shard serve 127.0.0.1:2377 --unix--> shard daemon --root /var/lib/shard-https
```

- Caddy stays up on 443. sslip.io resolves the name to the box's own address, so Let's Encrypt
  issues a real certificate with no DNS record of ours. Port 80 is shut at the Hetzner firewall,
  so Caddy gets the certificate by TLS-ALPN-01 on 443. It keeps no access log.
- The daemon and serve run from their own binaries in `/opt/shard-https/bin`, over their own root
  `/var/lib/shard-https`. A `make devbox-sync`, an `itest` or an e2e run replaces
  `/usr/local/bin/shard` and never touches these. The systemd unit's `/var/lib/shard` is not used.
- The pair runs only during a test window. Every daemon's egress proxy binds 30080 and 30443 on
  the bridge gateway whatever its root, so a window and an e2e run cannot share the box. The units
  have no `[Install]` section, so a boot never opens a window.
- serve runs as root with an empty capability set. A `shard` group would let it run unprivileged,
  but it would also turn the socket of every test daemon on the box from 0600 to 0660.
- Outside a window, Caddy answers 502.

## Deploy

Once per box, in this order:

1. Apply the 443 rule of `hcloud_firewall.shard` from nairi-infra main, from a saved plan:

   ```
   cd infra
   terraform plan -target=hcloud_firewall.shard -var-file=terraform.tfvars -var-file=secrets.tfvars -out=fw.plan
   ```

   Apply only when it says `Plan: 0 to add, 1 to change, 0 to destroy.`, and only that saved plan:
   `terraform apply fw.plan`. Any other count changes more than the one rule, so stop and find out
   why before anything is applied. Then commit `infra/terraform.tfstate` in a nairi-infra PR, as the
   state lives in that repo.
2. In nairi-infra: `make provision TARGET=shard-https`. The playbook runs only on the host with
   `slug=shard-devbox2`.
3. In this repo: `make devbox-sync DEVBOX=devbox-shard2 DEVBOX_PREFIX=/opt/shard-https/bin`.
4. Mint the client token: `ssh devbox-shard2 sudo shard-https token`. It writes
   `/etc/shard-https/sdk.env`, root 0600, and prints only the path and the scopes.
5. Copy it to the Mac without showing it:

   ```
   (umask 077; mkdir -p ~/.shard && ssh devbox-shard2 sudo cat /etc/shard-https/sdk.env > ~/.shard/devbox-shard2.env)
   ```

The token lives in those two files only. It never goes in a repo, a PR, a ticket or a chat.

## A test window

Take the box first: nobody else runs a daemon, an itest or an e2e on it during the window.

```
ssh devbox-shard2 sudo shard-https start
set -a; . ~/.shard/devbox-shard2.env; set +a
shard version
ssh devbox-shard2 sudo shard-https stop
```

`start` refuses while another daemon serves the proxy ports, and waits until serve answers. `stop`
removes every sandbox of the window, stops serve and the daemon, and drops the bridge `shard0` and
the two `shard` nft tables. It keeps them, and says why, while the bridge has a port or another
daemon serves the proxy. If it cannot list the sandboxes, it exits 1 and leaves the daemon up with
its records, so a second `stop` can finish. `sudo shard-https status` prints the state of the three
units.

To put a new build behind the endpoint, stop the window, run step 3 of the deploy, and start it again.

## Rotate the token

```
ssh devbox-shard2 sudo shard-https token
```

It revokes every earlier token of the name, mints a new one and rewrites the env file. serve checks
the ledger on every request, so the old token is refused at once and nothing restarts. Copy the
file to the Mac again, as in step 5.

The default name is `sdk`. Its scopes are `sandbox:read`, `sandbox:write`, `sandbox:delete` and
`exec`, so it has no `secret:*` and no `policy:*`. Another name takes its own scopes,
`sudo shard-https token ci sandbox:read`, and gets its own file.

## Tear down

In nairi-infra:

```
cd provision && uv run ansible-playbook playbooks/shard-https.yml -e shard_https_state=absent
```

It closes the window, removes the units, the script, the root, the binaries and the env files, and
purges Caddy with its certificates. Delete `~/.shard/devbox-shard2.env` on the Mac. The 443 rule can
stay, because nothing listens there, or go in a nairi-infra PR.

## Limits

- Let's Encrypt issues at most 5 certificates for the same name in 7 days. A window, a new build or
  a rotation reuses the certificate, but every teardown deletes it, so the next deploy asks for a
  new one.
- The front sees every client as Caddy's own connection, so its bound of 32 connections per source
  applies to all clients together ([Put HTTPS in front](https://useshards.com/docs/guides/remote/#3-put-https-in-front)).
