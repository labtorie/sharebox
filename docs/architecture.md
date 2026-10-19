# Architecture

## Components

```
apps/desktop (Wails UI) ─┐
cmd/sharebox-cli ────────┴─► internal/host.Engine
                               ├─ internal/fileserver   one http.Handler = the whole guest API
                               │    └─ internal/fsroot  os.Root confinement
                               ├─ internal/coord        WebSocket to the coordination server
                               ├─ internal/rtc          pion PeerConnections → internal/tunnel
                               ├─ internal/network      port mapping, STUN/TURN checks, watcher
                               └─ direct HTTPS listener (only once verified)

cmd/sharebox-server
  ├─ services/signaling   guest web app, /ws/host, /ws/guest, probe, TURN credentials
  │    └─ direct_acme.go  authoritative DNS + ACME DNS-01 (optional)
  └─ services/relay       pion/turn (STUN + TURN, UDP/TCP/TLS)
```

**Transport independence.** `fileserver.Server` is an `http.Handler`. The
direct listener serves it over TLS. `tunnel.Session` turns DataChannel frames
into `*http.Request`s and feeds the same handler, so P2P and relay behave
exactly like direct HTTPS. The conformance suite (`internal/conformance`) runs
unchanged over every transport to enforce this.

## Session lifecycle

1. **Start**: open the folder as an `os.Root`, generate a 256-bit access key,
   and create the file service, a loopback-only preview server and one UDP
   socket for all ICE traffic.
2. **Port mapping** (if enabled): PCP → NAT-PMP → UPnP for the UDP port, then
   the same port on TCP for HTTPS. The mapped address is advertised as an ICE
   candidate. Mappings are renewed at half their lifetime and deleted on stop.
3. **Register** with the coordination server: it gets `SHA-256(SignalToken)`
   and returns the session id, the link URL and ICE servers with short-lived
   TURN credentials.
4. **Verify**:
   * P2P: STUN binding from one socket to several servers. Differing mapped
     ports mean symmetric NAT, reported as a warning.
   * Relay: a real TURN allocation.
   * Direct: certificate (CSR → server → ACME) → HTTPS listener → the server
     connects back to *the IP the host connected from* and checks an HMAC of a
     nonce → `set-direct`.
   The link is shown only once at least one transport has been verified.
5. **Guests** load `/s/{id}`; the key in the URL fragment never reaches any
   server. The page joins with `SignalToken`, tries the direct URL
   (`/api/ping`, 3.5 s) and otherwise negotiates WebRTC. ICE prefers host,
   then server-reflexive, then relay candidates.
6. **Resilience**: the coordination client reconnects with backoff and
   *resumes* the session with its host token, so the link survives network
   changes, sleep and server restarts within the grace period (15 min). The
   watcher detects interface/gateway changes and wake from sleep, then
   reconnects and re-maps ports. Established DataChannels keep working while
   signalling is down. Guests renegotiate automatically if a DataChannel drops.
7. **Stop**: revoke all tokens, unregister (guests are told the share ended),
   close peers and listeners, delete port mappings and remove incomplete
   uploads. **Regenerate link** creates a new key *and* a new session id;
   the old link stops working immediately.

## Tunnel protocol

Defined in `shared/protocol/tunnel.go` and implemented in Go
(`internal/tunnel`) and TypeScript (`apps/guest-web/src/lib/tunnel.ts`). One
ordered, reliable DataChannel carries multiplexed streams; each message is a
frame with a 9-byte header (type, stream id, sequence number).

* Binary DATA frames of at most 16 KiB (the interoperable SCTP message size).
  No base64.
* Credit-based flow control per stream (1 MiB window, WINDOW frames
  acknowledge consumed bytes), plus global backpressure on `bufferedAmount`.
  Memory stays bounded on both ends regardless of file size.
* Sequence numbers detect lost or reordered frames. END carries the total byte
  count, so a truncated stream errors instead of silently succeeding.
* RESET cancels a stream in either direction; PING/PONG detects dead peers.
* Recovery happens at the HTTP layer: downloads resume with `Range`, uploads
  with `GET /api/uploads/{id}` → offset.

## Upload protocol

`POST /api/uploads` reserves disk space and creates a private temp file in the
share's hidden `.sharebox-uploads` folder. Chunks (8 MiB) are sent with
`PUT ?offset=N` and an `X-Chunk-SHA256` header. A chunk is committed
all-or-nothing: on a digest mismatch or a broken connection the file is
truncated back. `complete` fsyncs, sets permissions and mtime, and renames
atomically into place; existing files are never overwritten
(`name (2).ext`).
