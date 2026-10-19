#!/usr/bin/env bash
# Builds the Linux desktop app and packages it as .deb and .tar.gz.
# Must run on Linux (Wails links against GTK 3 and WebKitGTK 4.1).
#
#   sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
#   go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0
#   scripts/package-linux.sh
#
# Output: dist/desktop/sharebox_<v>_<arch>.deb and Sharebox-<v>-linux-<arch>.tar.gz
set -euo pipefail
cd "$(dirname "$0")/.."
[ "$(uname)" = Linux ] || { echo "run this on Linux" >&2; exit 1; }
VERSION="${VERSION:-1.0.0}"
VERSION="${VERSION#v}" # tags look like v1.2.3
WAILS="${WAILS:-$(command -v wails || echo "$HOME/go/bin/wails")}"
case "$(uname -m)" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
LDFLAGS="-s -w"
[ -n "${SHAREBOX_DEFAULT_SERVER:-}" ] && LDFLAGS="$LDFLAGS -X sharebox/internal/config.DefaultServerURL=${SHAREBOX_DEFAULT_SERVER%/}"

echo "==> guest web app"
(cd apps/guest-web && npm ci --no-audit --no-fund && npm run build)
echo "==> desktop app (linux/$ARCH, WebKitGTK 4.1)"
(cd apps/desktop && "$WAILS" build -clean -platform "linux/$ARCH" -tags webkit2_41 -ldflags "$LDFLAGS")
BIN=apps/desktop/build/bin/Sharebox
[ -x "$BIN" ] || { echo "build produced no binary" >&2; exit 1; }
mkdir -p dist/desktop

echo "==> .deb"
ROOT=$(mktemp -d)
install -Dm755 "$BIN" "$ROOT/usr/bin/sharebox"
install -Dm644 deploy/linux-desktop/sharebox.desktop "$ROOT/usr/share/applications/sharebox.desktop"
install -Dm644 apps/desktop/build/appicon.png "$ROOT/usr/share/icons/hicolor/512x512/apps/sharebox.png"
mkdir -p "$ROOT/DEBIAN"
cat > "$ROOT/DEBIAN/control" <<CTRL
Package: sharebox
Version: $VERSION
Section: net
Priority: optional
Architecture: $ARCH
Depends: libgtk-3-0, libwebkit2gtk-4.1-0
Maintainer: Sharebox <noreply@sharebox.invalid>
Homepage: https://github.com/labtorie/sharebox
Description: One-click folder sharing over the internet
 Share a local folder with friends; they open a link in any browser to
 browse, download and upload. Peer-to-peer via WebRTC with automatic
 fallbacks; no port forwarding needed.
CTRL
DEB="dist/desktop/sharebox_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$ROOT" "$DEB" >/dev/null
rm -rf "$ROOT"
echo "    $DEB"

echo "==> .tar.gz"
STAGE=$(mktemp -d)
D="$STAGE/Sharebox-$VERSION-linux-$ARCH"
mkdir -p "$D"
cp "$BIN" "$D/sharebox"
cp deploy/linux-desktop/sharebox.desktop apps/desktop/build/appicon.png "$D/"
cat > "$D/README.txt" <<TXT
Sharebox for Linux
Requires GTK 3 and WebKitGTK 4.1 (Ubuntu 22.04+, Debian 12+, Fedora 39+):
  Ubuntu/Debian:  sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0
  Fedora:         sudo dnf install gtk3 webkit2gtk4.1
Run:  ./sharebox
On Ubuntu/Debian the .deb package is easier: sudo apt install ./sharebox_${VERSION}_${ARCH}.deb
TXT
tar -C "$STAGE" -czf "dist/desktop/Sharebox-$VERSION-linux-$ARCH.tar.gz" "$(basename "$D")"
rm -rf "$STAGE"
echo "    dist/desktop/Sharebox-$VERSION-linux-$ARCH.tar.gz"
