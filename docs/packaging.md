# Building and packaging

## Prerequisites

| Tool | Needed for | Install |
|---|---|---|
| Go ≥ 1.27 | everything | https://go.dev/dl or `brew install go` |
| Node ≥ 22 | web UIs | https://nodejs.org |
| Wails CLI v2.17 | desktop app | `go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0` |
| Xcode Command Line Tools | macOS app | `xcode-select --install` |
| makensis | Windows installer (optional) | `brew install makensis` / `apt install nsis` |
| WebView2 runtime | running on Windows | preinstalled on Windows 11; the installer bootstraps it on Windows 10 |

## Commands

| | |
|---|---|
| `scripts/dev.sh` | Local server + desktop app with hot reload |
| `scripts/build.sh` | All artifacts this machine can build, into `dist/` |
| `scripts/build.sh web server cli` | Just the guest app and Go binaries |
| `scripts/build.sh desktop-mac` | `Sharebox-<v>-macos-universal.dmg` (Apple Silicon + Intel) |
| `scripts/build.sh desktop-win` | `Sharebox-<v>-windows-x64.exe` (+ `-setup.exe` with makensis) |
| `VERSION=1.2.3 scripts/build.sh` | Set the version in file names |
| `SHAREBOX_DEFAULT_SERVER=https://share.example.org scripts/build.sh` | Build the shared server address into the desktop app and CLI, so hosts need no setup (it can still be changed in Settings). Alternatively set `DefaultServerURL` in `internal/config/config.go`. |

The guest web app is compiled into `internal/guestweb/dist` and embedded in
both the server and the host, so build `web` first whenever it changes.

## Platform status

| Target | Built here | Executed here |
|---|---|---|
| macOS Apple Silicon (arm64) | ✅ (universal binary) | ✅ launched and driven via `wails dev` |
| macOS Intel (x86_64) | ✅ (universal binary) | ❌ no Intel Mac available |
| Windows x64 exe + NSIS installer | ✅ cross-compiled | ❌ no Windows machine available |
| Server/CLI linux, macOS, windows (amd64/arm64) | ✅ | macOS arm64 only |

## macOS signing and notarization

The DMG produced here is **ad-hoc signed**. Gatekeeper will block it on other
Macs ("cannot be opened because the developer cannot be verified"); users can
right-click → Open, but for distribution you should sign and notarize:

1. Join the Apple Developer Program and create a **Developer ID Application**
   certificate in Keychain.
2. Sign with the hardened runtime:
   ```bash
   codesign --deep --force --options runtime --timestamp \
     --sign "Developer ID Application: Your Name (TEAMID)" \
     apps/desktop/build/bin/Sharebox.app
   ```
3. Create the DMG (as `scripts/build.sh` does), sign it too, then notarize and
   staple:
   ```bash
   xcrun notarytool submit dist/desktop/Sharebox-1.0.0-macos-universal.dmg \
     --apple-id you@example.com --team-id TEAMID --password <app-specific-password> --wait
   ```
   ```bash
   xcrun stapler staple dist/desktop/Sharebox-1.0.0-macos-universal.dmg
   ```

Sharebox needs no special entitlements outside the App Store sandbox. The app
declares `NSLocalNetworkUsageDescription`, so macOS asks once before it talks
to the router (port mapping) and LAN peers. The first time the app listens for
direct connections, the macOS firewall may ask to allow incoming connections.

## Windows signing

Unsigned executables trigger SmartScreen warnings. Sign both the app and the
installer with an Authenticode certificate (an EV certificate gets
SmartScreen reputation immediately), e.g.:

```bash
signtool sign /fd sha256 /tr http://timestamp.digicert.com /td sha256 /a dist\desktop\Sharebox-1.0.0-windows-x64-setup.exe
```

From macOS or Linux, `osslsigncode` can do the same. Windows Defender Firewall
asks for permission the first time Sharebox accepts direct connections.

## Desktop app behaviour notes

* Settings, device identity and the direct-HTTPS certificate live in
  `~/Library/Application Support/Sharebox` (macOS) or `%AppData%\Sharebox`
  (Windows). `SHAREBOX_CONFIG_DIR` overrides this; `SHAREBOX_SERVER` presets
  the server URL.
* Closing the window while sharing minimises it when "Keep sharing when the
  window is closed" is on (the default); otherwise sharing stops and the app
  quits. Quitting always stops sharing, removes port mappings and deletes
  incomplete uploads. Wails v2 has no system tray API, so there is no tray
  icon; launching Sharebox again focuses the running instance.
