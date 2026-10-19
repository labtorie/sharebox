# Security model

Sharebox exposes part of a local filesystem to the internet, so guests are
treated as untrusted at every layer.

## Credentials and links

* Link format: `https://<server>/s/<session-id>#k=<access-key>`.
  * Session id: 120 random bits. It is visible to the coordination server.
  * Access key: 256 random bits, carried in the **URL fragment**. Browsers
    never send fragments to servers, so the key appears in no request, server
    log or `Referer` header (`Referrer-Policy: no-referrer` is set as well).
    The guest page moves it to `sessionStorage` and removes it from the
    address bar and history with `history.replaceState`.
* The coordination server stores only `SHA-256(HMAC(key, "sharebox-signal-v1"))`.
  That lets it gate who may *signal* a host, but the value cannot be turned
  into the key, and file access always requires the key itself, verified by
  the host over the encrypted channel.
* The host returns `HMAC(key, DTLS fingerprint)` with its WebRTC answer, and
  the guest refuses answers that don't verify. A signalling server that tries
  to substitute its own DTLS endpoint (MITM) is detected.
  *Limitation:* the guest web app is served by the coordination server, so a
  fully compromised server could serve malicious JavaScript. Operate the
  server as trusted infrastructure.
* After presenting the key (and the optional password), guests receive a
  12-hour, HMAC-signed, capability token bound to the session. Tokens carry an
  epoch: **Regenerate link** and **Stop** rotate it, revoking everyone at once.
  Individual guests can be revoked too. Session expiry is enforced by both the
  host and the server.
* Passwords are hashed with scrypt, kept in memory only and never persisted.
  Attempts are limited per connection and share-wide, so rotating WebRTC
  connections doesn't bypass the limit.

## Filesystem confinement

* Guest paths are validated lexically, the same way on every OS (`fsroot.Clean`).
  Rejected: `..`, absolute paths, backslashes, NUL and control characters,
  `: * ? " < > |` (including NTFS alternate data streams), trailing dots and
  spaces, and Windows device names (`CON`, `NUL`, `COM1`, …).
* Every access goes through Go's `os.Root`. Paths are resolved relative to an
  open directory handle and cannot escape via `..` or symlinks. Resolution is
  handle-relative (`openat`, `NtCreateFile` with a root handle), so swapping a
  directory for a symlink between check and use doesn't work
  (`TestSwappedDirectoryRace`).
* Symlinks inside the share work; links pointing outside are hidden from
  listings, searches and ZIPs. Deleting a symlink never touches its target.
  Devices, sockets and FIFOs are never served.
* File types are checked on the *opened handle*, not the path.
* The internal upload folder can't be addressed even through aliases such as
  Windows 8.3 short names or case variations (it is compared with
  `os.SameFile`).
* Absolute host paths never appear in responses or errors.
* The desktop app refuses to share a whole disk or the home folder.

## Transport security

* Direct mode is HTTPS only, with a publicly trusted certificate whose private
  key never leaves the host, and HSTS. The plain-HTTP preview server binds to
  127.0.0.1 only. No unencrypted service is ever exposed publicly.
* WebRTC is always DTLS-encrypted end to end, including through TURN.
* The coordination server requires HTTPS except for `localhost` development.

## Abuse resistance

* Rate limits: per-connection API requests, authentication attempts, and
  WebSocket connections per IP on the server.
* Concurrency limits on transfers and streams. Upload limits: per-file size,
  per-session quota, a minimum-free-disk reserve with space reserved at upload
  creation (`507 Insufficient Storage`), and caps on simultaneous upload
  sessions. Idle uploads are reaped.
* Deletion is **off by default**. Uploads never overwrite existing files.
* Downloads are always `Content-Disposition: attachment` with
  `Content-Security-Policy: sandbox`, and HTML/JS/XML is served as
  `application/octet-stream`. Uploaded content cannot run as a page on the
  host's origin.
* The reachability probe only connects to the IP the requesting host is
  connected from, on ports ≥ 1024, so it can't be used for scanning or SSRF.
* The TURN relay refuses to relay to private, loopback, link-local and CGNAT
  addresses (outside `-dev`), caps allocations per credential, and uses
  time-limited credentials.
* State-changing API calls need an `Authorization` header. Cookies (used only
  for native downloads in direct mode) are `HttpOnly; Secure; SameSite=Strict`
  and accepted for GET only, which prevents CSRF.

## Logging

Logs never include access keys, tokens, passwords or filesystem paths.
