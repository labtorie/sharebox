# Sharebox

One-click folder sharing over the internet. The host picks a folder, presses
**Start Sharing** and sends a link. The guest opens it in any modern browser to
browse, download and upload files. Guests install nothing and create no account,
and the host never has to set up port forwarding on the router.

```
 Host (desktop app)                Coordination server             Guest (browser)
 ───────────────────               ───────────────────             ───────────────
 file service  ◄──── direct HTTPS (trusted cert, port mapped) ────────────────┐
      ▲                                                                       │
      │  WebRTC DataChannel (DTLS) — peer-to-peer via ICE/STUN ───────────────┤
      │                              │                                        │
      └──── or relayed through ──► TURN relay ◄───────────────────────────────┘
            signalling, links, probes ◄──► WebSocket ◄──► guest web app
```

* **Direct HTTPS**: used when router port mapping works *and* the coordination
  server has confirmed the host is reachable from the internet. Needs the
  optional DNS zone on the server (see below).
* **Peer-to-peer WebRTC**: used when ICE finds a direct path (same LAN, STUN
  hole punching).
* **Relay**: used automatically when nothing else works. Traffic is end-to-end
  DTLS-encrypted, so the relay can't read it and never stores it.

All three transports carry the **same** HTTP API into the **same** Go handler.
Permissions, path checks, uploads and downloads are implemented exactly once.

## What's in the repository

| Path | What |
|---|---|
| `apps/desktop/` | Wails desktop app (Go backend + React/TypeScript/Tailwind UI) |
| `apps/guest-web/` | Guest web app (React/TypeScript/Tailwind), embedded into the Go binaries |
| `cmd/sharebox-server/` | Coordination server: guest web app, signalling, TURN relay, reachability probe, optional DNS + ACME |
| `cmd/sharebox-cli/` | Headless host (servers, NAS, automation) |
| `internal/fsroot/` | Filesystem confinement (`os.Root`, path validation) |
| `internal/fileserver/` | The guest API: list, search, Range downloads, ZIP streaming, resumable uploads, permissions |
| `internal/tunnel/` | HTTP-over-DataChannel framing with flow control (Go side) |
| `internal/rtc/` | pion/webrtc host endpoint, plus a Go guest dialer used in tests |
| `internal/network/` | PCP, NAT-PMP and UPnP port mapping; STUN and TURN checks; network/sleep watcher |
| `internal/host/` | Session orchestration and connectivity verification (`Engine`) |
| `internal/coord/` | Host ↔ coordination server client (reconnect + resume) |
| `services/signaling/`, `services/relay/` | Server components |
| `shared/protocol/` | Wire formats (mirrored in `apps/guest-web/src/lib/protocol.ts`) |
| `tests/` | WebRTC/TURN/direct-HTTPS integration tests, TS↔Go interop server |
| `docs/` | Architecture, security, deployment, testing, packaging |

## Quick start (everything on one machine)

Requirements: Go 1.27+, Node 22+, and for the desktop app Wails v2.17
(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`) plus Xcode
Command Line Tools on macOS or WebView2 on Windows.

```bash
scripts/build.sh web server cli
```

```bash
bin/sharebox-server -dev
```

The `-dev` server runs at http://127.0.0.1:8080 with a TURN relay on 127.0.0.1:3478.

```bash
bin/sharebox-cli -server http://127.0.0.1:8080 -folder ~/Desktop/test -no-portmap
```

Open the printed link in a browser. To get the desktop app with hot reload
against the same local server:

```bash
scripts/dev.sh
```

`-dev` mode only works for guests on the same computer (http://localhost).
Sharing with friends needs a deployed coordination server.

## Sharing with real people: deploy a coordination server

Sharebox needs a small public server for links, signalling and the relay.
This build has the shared server `https://194-35-112-81.sslip.io` built in
(`DefaultServerURL` in `internal/config/config.go`), so hosts don't have to
configure anything. It is a full server on a VPS (signalling, guest web app
and TURN relay), installed as described below. To run your own server
instead, you need:

* a VPS with a public IPv4 address,
* a domain name (e.g. `share.example.org`) pointing at it,
* open ports: TCP 443 and 80, UDP and TCP 3478, UDP 49160–49999.

The fastest way is the Ubuntu package (`scripts/build.sh server-pkg`). Copy
`dist/server-packages/sharebox-server-<v>-linux-amd64.tar.gz` to the server,
unpack it and run:

```bash
sudo ./install.sh
```

Docker Compose and plain systemd setups are also in `deploy/`.

Then enter `https://share.example.org` under **Settings → Sharebox server**
in the desktop app. Full instructions, including the optional direct-HTTPS
DNS zone and running coturn instead of the embedded relay, are in
[docs/deploy-server.md](docs/deploy-server.md).

## Building release artifacts

```bash
scripts/build.sh
```

This produces:

* `dist/sharebox-server/*`, `dist/sharebox-cli/*`: Linux, macOS and Windows (amd64/arm64).
* `dist/desktop/Sharebox-<v>-macos-universal.dmg`: Apple Silicon + Intel, ad-hoc signed.
* `dist/desktop/Sharebox-<v>-windows-x64.exe` and `-setup.exe` (the installer needs `makensis`).

The macOS signing/notarization and Windows signing steps are described in
[docs/packaging.md](docs/packaging.md).

## Tests

```bash
scripts/test.sh
```

This runs the Go unit and integration suites (race detector), an 11 GiB
streaming test, the TypeScript↔Go tunnel interop tests, and browser
end-to-end tests in Google Chrome. See [docs/testing.md](docs/testing.md) for
what each suite covers and which scenarios need real infrastructure.

## Known limitations

* **Infrastructure is required.** Internet sharing needs the coordination
  server above. Without a TURN relay, guests behind strict NATs (some mobile
  carriers, corporate networks) cannot connect, and the host UI says so.
* **Direct HTTPS is optional and infrastructure-heavy.** It needs a DNS zone
  delegated to the server and an ACME CA. Let's Encrypt rate limits (50
  certificates per registered domain per week by default) cap how many *new*
  devices a single deployment can enable per week. Certificates are reused for
  their lifetime. Hosts behind carrier-grade NAT never qualify; the
  reachability probe detects this and falls back to WebRTC.
* **No system tray.** Wails v2 has no tray API. With "Keep sharing when the
  window is closed" on, closing the window minimises it; the Dock or taskbar
  icon, or launching Sharebox again, brings it back.
* **Downloads over WebRTC** stream to disk through a service worker (Chrome,
  Edge, Firefox, Safari). Where service workers are unavailable (Firefox
  private windows, some embedded browsers), files are assembled in memory
  first, and the UI warns for very large files. Direct HTTPS uses the browser's
  native download manager.
* **Single-instance server.** Sessions live in memory on one server process;
  horizontal scaling would need shared session state.
* **IPv4 only** for port mapping and direct HTTPS. WebRTC itself uses whatever
  ICE finds.
