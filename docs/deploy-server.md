# Deploying the coordination server

One `sharebox-server` process provides the guest web app, signalling, the
reachability probe, the TURN/STUN relay and, optionally, DNS and certificates
for direct HTTPS. It stores no files and keeps sessions in memory.

## Requirements

* A Linux VPS with a **public IPv4 address**. The relay must not sit behind
  NAT; if your provider uses 1:1 NAT (e.g. AWS Elastic IP), set
  `SHAREBOX_TURN_PUBLIC_IP` to the public address.
* A domain, e.g. `share.example.org`, with an A record pointing at the server.
* Firewall: TCP 80, 443; UDP and TCP 3478; UDP 49160–49999 (relay ports;
  adjustable with `-turn-ports`).
* Resources: signalling is tiny. Relay bandwidth is what matters: every relayed
  transfer passes through the server at full speed in both directions.

## Quickest: Ubuntu/Debian package

`scripts/build.sh server-pkg` creates
`dist/server-packages/sharebox-server-<v>-linux-{amd64,arm64}.tar.gz`, which
contains the binary, `install.sh`, the systemd unit and a short README.

```bash
scp dist/server-packages/sharebox-server-1.0.0-linux-amd64.tar.gz root@SERVER_IP:
```

```bash
ssh root@SERVER_IP
```

```bash
tar xzf sharebox-server-1.0.0-linux-amd64.tar.gz && cd sharebox-server-1.0.0-linux-amd64
```

```bash
sudo ./install.sh
```

The installer asks for the domain and e-mail (or pass `--domain … --email …
--yes`). It then:
* detects the public IP,
* checks that DNS points here,
* generates the relay secret into `/etc/sharebox/env` (mode 600),
* installs and starts the systemd service (as an unprivileged dynamic user),
* opens ufw if it is active,
* waits until `https://<domain>/healthz` answers with a valid certificate.

`--upgrade` replaces only the binary; `--uninstall [--purge]` removes it.
Tested on Ubuntu 24.04 (arm64, systemd in a container): install, re-run,
upgrade, automatic restart after a crash, and uninstall/purge. A real Let's
Encrypt certificate needs a real domain and was not part of that test.

## Docker hosting platforms (PaaS)

Platforms that build a `Dockerfile` from your repository and expose one HTTP
port behind their own HTTPS (Amvera, Render, Railway, Fly.io HTTP services…)
are supported by the root `Dockerfile`:

| Setting | Value |
|---|---|
| Dockerfile | `Dockerfile` (repository root) |
| Container port | `8080` |
| Persistent mount | `/data` |
| Environment | `SHAREBOX_PUBLIC_URL=https://<your app domain>` |
| Run mode | single instance (sessions are in memory) |

The image already sets plain HTTP on :8080, trusts the platform's
`X-Forwarded-For` (the rightmost public address is used), and hands out
public STUN servers (Google, Cloudflare).

**Limitation:** these platforms don't forward UDP, so the TURN relay can't run
there. Guests connect peer-to-peer, which works for most home and office
networks. Guests whose network blocks it (some mobile carriers, strict
corporate firewalls, symmetric NAT on both ends) can't connect, and the host
app shows "Relay: unavailable". To close that gap, run a relay on any VPS
(the Ubuntu package with `SHAREBOX_TURN=1`, or coturn with
`static-auth-secret`), then set `SHAREBOX_TURN_URLS` and
`SHAREBOX_TURN_SECRET` on the platform.

Verified locally behind a TLS-terminating, WebSocket-passing reverse proxy
that appends an internal hop to `X-Forwarded-For`: host registration, guest
page, peer-to-peer connection in Chrome and an upload. The image build itself
was not run (Docker Desktop was unavailable); check the platform's build log
on the first deploy.

## Option A — Docker Compose

```bash
cd deploy && cp .env.example .env
```

Generate a TURN secret:

```bash
openssl rand -base64 32
```

Edit `.env` (public URL, public IP, TURN secret, ACME e-mail), then start it:

```bash
docker compose up -d
```

```bash
curl https://share.example.org/healthz
```

The compose file uses host networking so the relay sees real client addresses.

## Option B — systemd

```bash
sudo install -m755 dist/sharebox-server/sharebox-server-1.0.0-linux-amd64 /usr/local/bin/sharebox-server
```

```bash
sudo install -d -m700 /etc/sharebox
```

```bash
sudo install -m600 deploy/.env.example /etc/sharebox/env
```

Edit `/etc/sharebox/env`, then:

```bash
sudo cp deploy/sharebox-server.service /etc/systemd/system/
```

```bash
sudo systemctl enable --now sharebox-server
```

## TLS options

| Flag | Use |
|---|---|
| `-autocert` | Built-in Let's Encrypt for the public URL host (needs ports 80/443 on this server) |
| `-tls-cert`, `-tls-key` | Your own certificate |
| `-http -trust-proxy` | Behind a TLS-terminating reverse proxy (Caddy, nginx). The proxy must pass WebSocket upgrades on `/ws/` and set `X-Forwarded-For`. |

## Relay options

* **Embedded relay** (`-turn`): pion/turn on UDP and TCP 3478, optional TURN over
  TLS with `-turn-tls :5349` (reuses the HTTPS certificate) for networks that
  only allow TLS. It refuses to relay into private networks.
* **External relay** (e.g. coturn): run coturn with
  `use-auth-secret` and `static-auth-secret=<secret>`, then start Sharebox with
  `-turn-secret <secret> -turn-urls "turn:relay.example.org:3478?transport=udp,turn:relay.example.org:3478?transport=tcp"`
  and without `-turn`.
* **No relay**: guests behind strict NATs won't be able to connect. Hosts see
  "Relay: unavailable".

## Optional: direct HTTPS for hosts

This lets guests connect straight to the host over HTTPS with a publicly
trusted certificate, no relay involved. It's optional; WebRTC covers the same
cases with slightly more overhead.

1. Pick a zone, e.g. `direct.example.org`, and delegate it to this server.
   At your DNS provider, create:
   ```
   ns1.example.org.          A    203.0.113.10
   direct.example.org.       NS   ns1.example.org.
   ```
2. Open UDP and TCP port 53 on the server.
3. Configure:
   ```
   SHAREBOX_DIRECT_ZONE=direct.example.org
   SHAREBOX_DNS_NS=ns1.example.org
   SHAREBOX_ACME_EMAIL=admin@example.org
   ```
4. Restart. Each host device gets a stable name such as
   `k3m9x2p7q4ab.direct.example.org`, always pointing at the address that
   device connects from. The host sends a CSR; the server completes the ACME
   DNS-01 challenge and returns the certificate. Host private keys never leave
   the host.

**Rate limits.** Let's Encrypt allows 50 new certificates per registered
domain per week (you can request a higher limit). Certificates last 90 days
and are reused, but each *new* device consumes one. Sharebox also caps
issuance at 3 per device per day. For a large user base, request a rate-limit
increase or use a commercial ACME CA (`-acme-directory`).

## Operations

* `GET /healthz` returns `{"ok":true,"sessions":N}`.
* State: `SHAREBOX_DATA_DIR` holds autocert certificates, the ACME account key
  and the direct-device registry. Back it up if you use direct HTTPS.
* Sessions are in memory. A restart disconnects signalling, but hosts
  reconnect automatically. Their links change only if the restart takes
  longer than the host's reconnect (the session is recreated) — hosts will show
  the new link.
* Scaling: one instance comfortably handles many thousands of idle sessions;
  relay bandwidth is the bottleneck. Several instances would need shared
  session state (not implemented).
