#!/usr/bin/env bash
# Builds every Sharebox artifact that the current machine can produce.
#
#   scripts/build.sh            # everything available
#   scripts/build.sh web server # selected targets: web server cli desktop-mac desktop-win
#
# Output goes to dist/. Desktop builds need Wails (go install
# github.com/wailsapp/wails/v2/cmd/wails@v2.17.0); macOS app bundles can
# only be built on macOS. The Windows app cross-compiles from macOS/Linux;
# its NSIS installer additionally needs makensis.
set -euo pipefail
export COPYFILE_DISABLE=1 # no macOS ._ files in tarballs
cd "$(dirname "$0")/.."
VERSION="${VERSION:-1.0.0}"
LDFLAGS="-s -w"
WAILS="${WAILS:-$(command -v wails || echo "$HOME/go/bin/wails")}"
TARGETS=("$@")
[ ${#TARGETS[@]} -eq 0 ] && TARGETS=(web server server-pkg cli desktop-mac desktop-win)
want() { [[ " ${TARGETS[*]} " == *" $1 "* ]]; }
mkdir -p dist bin

if want web; then
  echo "==> guest web app"
  (cd apps/guest-web && npm ci --no-audit --no-fund && npm run build)
fi

build_go() { # name pkg
  local name=$1 pkg=$2
  for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do
    local os=${t%/*} arch=${t#*/} ext=""
    [ "$os" = windows ] && ext=".exe"
    echo "    $name $os/$arch"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
      -o "dist/$name/$name-$VERSION-$os-$arch$ext" "./$pkg"
  done
  go build -trimpath -ldflags "$LDFLAGS" -o "bin/$name" "./$pkg" # native, for local use/tests
}

if want server; then echo "==> coordination server"; build_go sharebox-server cmd/sharebox-server; fi
if want server-pkg; then
  echo "==> Ubuntu/Debian server packages"
  for arch in amd64 arm64; do
    bin="dist/sharebox-server/sharebox-server-$VERSION-linux-$arch"
    [ -f "$bin" ] || CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$bin" ./cmd/sharebox-server
    name="sharebox-server-$VERSION-linux-$arch"
    stage=$(mktemp -d)
    mkdir "$stage/$name"
    cp "$bin" "$stage/$name/sharebox-server"
    cp deploy/ubuntu/install.sh deploy/ubuntu/README.txt deploy/sharebox-server.service "$stage/$name/"
    chmod 755 "$stage/$name/install.sh" "$stage/$name/sharebox-server"
    mkdir -p dist/server-packages
    TARFLAGS=()
    tar --version 2>/dev/null | grep -q bsdtar && TARFLAGS=(--no-xattrs --no-mac-metadata) # clean archives from macOS
    tar "${TARFLAGS[@]}" -C "$stage" -czf "dist/server-packages/$name.tar.gz" "$name"
    rm -rf "$stage"
    echo "    dist/server-packages/$name.tar.gz"
  done
fi

if want cli; then echo "==> headless host"; build_go sharebox-cli cmd/sharebox-cli; fi

if want desktop-mac; then
  if [ "$(uname)" = Darwin ]; then
    echo "==> desktop app (macOS universal: Apple Silicon + Intel)"
    (cd apps/desktop && "$WAILS" build -clean -platform darwin/universal -ldflags "$LDFLAGS")
    mkdir -p dist/desktop
    STAGE=$(mktemp -d)
    cp -R apps/desktop/build/bin/Sharebox.app "$STAGE/"
    ln -s /Applications "$STAGE/Applications"
    rm -f "dist/desktop/Sharebox-$VERSION-macos-universal.dmg"
    hdiutil create -volname "Sharebox" -srcfolder "$STAGE" -ov -format UDZO \
      "dist/desktop/Sharebox-$VERSION-macos-universal.dmg" >/dev/null
    rm -rf "$STAGE"
    echo "    dist/desktop/Sharebox-$VERSION-macos-universal.dmg (ad-hoc signed; see docs/packaging.md to sign + notarize)"
  else
    echo "==> skipping macOS desktop build (requires macOS)"
  fi
fi

if want desktop-win; then
  echo "==> desktop app (Windows x64)"
  NSIS=""
  command -v makensis >/dev/null && NSIS="-nsis"
  # makensis from Homebrew aborts (std::bad_alloc) under some locales; force UTF-8.
  (cd apps/desktop && LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 "$WAILS" build -clean -platform windows/amd64 -ldflags "$LDFLAGS" $NSIS)
  mkdir -p dist/desktop
  cp apps/desktop/build/bin/Sharebox.exe "dist/desktop/Sharebox-$VERSION-windows-x64.exe"
  if [ -n "$NSIS" ]; then
    cp apps/desktop/build/bin/*installer*.exe "dist/desktop/Sharebox-$VERSION-windows-x64-setup.exe"
  else
    echo "    (install makensis to also produce an installer)"
  fi
fi
echo "==> done"; find dist -type f | sort
