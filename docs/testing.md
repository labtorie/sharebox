# Testing

```bash
scripts/test.sh
```

| Suite | Command | What it proves |
|---|---|---|
| Filesystem confinement | `go test ./internal/fsroot` | Path validation (traversal, Windows device names, ADS, trailing dots); symlink escapes blocked and hidden; a directory swapped for a symlink after listing cannot be followed; internal folder unreachable; root cannot be deleted |
| Guest API over direct HTTPS | `go test ./internal/fileserver` | The conformance suite (below) over real TLS, plus an **11 GiB** sparse-file download with peak heap ≈ 60 MiB |
| Tunnel protocol | `go test ./internal/tunnel` | Conformance suite over the DataChannel framing; 1 GiB up/down with bounded memory; a dropped frame is detected; link loss then resume with Range gives identical bytes; cancellation stops the host; a peer ignoring flow control is cut off |
| Port mapping | `go test ./internal/network` | PCP, NAT-PMP fallback and "router doesn't answer", against fake routers; CGNAT detection |
| Security primitives | `go test ./internal/security` | Token tampering, expiry, per-guest and global revocation; scrypt; HMAC domain separation |
| Coordination server | `go test ./services/signaling` | Joining requires the signal token; unknown sessions; signal routing; session hijack with a forged host token fails; unregistering notifies guests; the probe refuses private targets; DNS zone answers (A/NXDOMAIN/REFUSED); foreign CSRs rejected |
| ACME (real CA) | `SHAREBOX_PEBBLE=… SHAREBOX_PEBBLE_SRC=… go test ./services/signaling -run Pebble` | Full DNS-01 issuance against [Pebble](https://github.com/letsencrypt/pebble); the issued certificate matches the host's own key |
| WebRTC end to end | `go test ./tests/integration` | Real coordination server, real TURN relay, real host engine and a pion WebRTC guest: the **whole conformance suite over P2P and again over relay-only ICE**; verified transport checks; Stop and Regenerate cut off guests and invalidate old links; wrong key refused; host reconnect keeps the link; transfers survive a signalling outage |
| Direct HTTPS end to end | same, with the Pebble env vars | Simulated router mapping → certificate from Pebble via the server → HTTPS listener → external probe → guest connects to `https://<label>.direct.test:<port>` with a trusted chain resolved via the server's DNS |
| TypeScript ⇄ Go interop | `cd apps/guest-web && SBX_INTEROP_BIN=… npx vitest run` | The browser tunnel client against the Go host: auth, exact-byte download larger than the window, concurrent Range streams, chunked SHA-256-verified upload via the real upload manager, mid-stream cancellation |
| Browser end to end | `cd apps/guest-web && npx playwright test` | **Google Chrome** against server, relay and the headless host: P2P browse, service-worker streamed download (hash verified), upload (hash verified), ZIP, new folder, search, delete, stop → "share has ended"; forced relay (`?transport=relay`) download and upload; password prompt; invalid and incomplete links |

The conformance suite (`internal/conformance`) covers unauthorized access,
browsing, traversal and symlink escapes, Range downloads, HTML not rendered
inline, upload round trips, no overwrite, hostile names, resumable uploads with
corrupted, misaligned, truncated and overflowing chunks, cross-guest
isolation, every permission, passwords, revocation, link rotation and expiry,
ZIP contents, search, 16 concurrent transfers, the disk-exhaustion guard and
upload size limits.

## Results at the time of writing (macOS 27, Apple M4 Pro)

All suites above were run and passed, including `go test -race`.
The desktop app was also checked manually through `wails dev`: start sharing,
verified link, connectivity panel, a guest uploading 200 MB (hash verified)
with live activity, and **New link** disconnecting the guest.

## What needs real infrastructure

These can't be fully automated on one machine and should be checked on a
deployment:

* **Real NAT traversal**: two machines on different home networks (P2P via
  STUN), a mobile hotspot or CGNAT carrier (relay expected), and a corporate
  network that blocks UDP (TURN over TCP/TLS).
* **Real router port mapping**: UPnP, NAT-PMP and PCP against physical
  routers. UPnP has no automated test; PCP and NAT-PMP are tested against
  protocol-accurate fakes.
* **Let's Encrypt production**: DNS delegation and real DNS-01 issuance
  (verified only against Pebble).
* **Sleep/wake and Wi-Fi switching** on real laptops (the watcher logic and
  host reconnect/resume are tested; actual OS events are not).
* **Windows host**: the Windows binaries are cross-compiled from macOS and were
  not executed. Filesystem rules for Windows paths are unit-tested on all
  platforms; `os.Root` provides the Windows confinement.
* **Safari / Firefox / mobile browsers**: the browser suite runs in Chrome
  only.
