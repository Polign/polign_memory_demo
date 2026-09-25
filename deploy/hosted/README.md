# Hosting the memory demo on the demo host

This is what runs behind <https://polign.com/memory-demo>. It shares the
`demo.polign.com` host with the Wikipedia demo and adds three services:

| unit | listens on | what it is |
| --- | --- | --- |
| `polign-memory-node-a` | 127.0.0.1:23400 | polign_db 0.7.1, cold-first, on `s3://polign-demo-wiki-en-uw1/memory-live` |
| `polign-memory-node-b` | 127.0.0.1:23402 | a second node on the same prefix |
| `polign-memory-demo` | 127.0.0.1:23300 | this repository in `-hosted` mode |

Caddy routes `demo.polign.com/memory/*` to the backend; the page itself is a
static file on polign.com. The browser signs in with the polign.com account
(Cognito) and sends the ID token as a bearer credential. The backend verifies
it, derives a namespace from the account subject, mints a polign_db key bound
to that namespace, and builds two agents, one per node. Every visitor's
memories live in the same collection, separated by the server-enforced
namespace.

## Install

```sh
# binaries: polign_db v0.7.1 with verified signed checksums, this repo cross-compiled
sudo install -m 0755 polign-server /opt/polign/bin/polign-server-0.7.1
sudo install -m 0755 polign-apikey /opt/polign/bin/polign-apikey-0.7.1
sudo install -m 0755 polign-memory-demo /opt/polign/bin/polign-memory-demo

sudo install -d -o polign -g polign -m 0750 /var/lib/polign/memory-a /var/lib/polign/memory-b /var/lib/polign/memory-demo
sudo install -d -m 0755 /etc/polign
sudo install -o root -g polign -m 0640 memory-demo.env.example /etc/polign/memory-demo.env   # then put the real key in it

sudo install -m 0644 polign-memory-node-a.service polign-memory-node-b.service polign-memory-demo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now polign-memory-node-a polign-memory-node-b polign-memory-demo
```

Then splice `Caddyfile.snippet` into `/etc/caddy.Caddyfile` and run
`caddy validate --config /etc/caddy.Caddyfile && systemctl reload caddy`.

Build the backend for the host from this repository:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o polign-memory-demo .
```

## Checks

```sh
curl -s http://127.0.0.1:23400/healthz; curl -s http://127.0.0.1:23402/healthz
curl -s http://127.0.0.1:23300/healthz
curl -s -o /dev/null -w '%{http_code}\n' https://demo.polign.com/memory/api/session   # 401 without a token
```

The per-namespace keys are cached in `/var/lib/polign/memory-demo/keys.json`.
Deleting that file only makes the backend mint fresh keys; the old records
stay in the bucket under `.auth/keys/` and still work.

## Limits

Each visitor gets 30 model turns an hour across both agents, messages up to
2,000 bytes, and a conversation that clears itself after 30 turns. At most
four model calls run at once across all visitors. Idle visitors are dropped
from memory after 30 minutes; their memories stay in the bucket.
