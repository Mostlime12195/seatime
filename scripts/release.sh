#!/bin/sh
# Build SeaTime for the six supported targets into ./dist and write
# dist/SHA256SUMS. With SEATIME_SIGNING_KEY set it also writes dist/RELEASE,
# the signed manifest that `seatime update` and the daemon's daily check
# install from; without it the build is unsigned and no client updates to it.
#
#   SEATIME_SIGNING_KEY=~/seatime-release.key VERSION=v0.1.0 scripts/release.sh
#   scripts/release.sh                 # unsigned, version from `git describe`
#   TARGETS="linux/amd64 windows/386" scripts/release.sh   # subset
#
# The macOS menu bar icon is Cocoa, so the two macOS binaries need cgo and
# a Mac to build on; the other four are plain Go and build anywhere.
#
#   On a Mac                 all six are built here.
#   Anywhere else            set SEATIME_DARWIN_DIR to a folder with the two
#                            macOS binaries built on a Mac by this script
#                            (the release-build workflow's artifact: its
#                            dist/ with SHA256SUMS and VERSION). They are
#                            checked against that SHA256SUMS and VERSION and
#                            copied in. The other four are built here; where
#                            the folder has them too, the two builds must be
#                            the same bytes (SEATIME_SKIP_REPRO=1 skips this).
#   Neither                  the macOS binaries are built here without the
#                            icon. A signed release refuses that unless
#                            SEATIME_ALLOW_NO_ICON=1.
#
# The signing key never leaves the machine that signs: a Mac or CI only
# builds, and this script signs here.
#
# Host everything in dist/ in one folder: the server's SEATIME_DL_DIR, or
# anywhere public with seatimed's SEATIME_DL_UPSTREAM pointing at it.
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
TARGETS=${TARGETS:-"windows/amd64 windows/386 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"}
OUT=${OUT:-dist}
KEY=${SEATIME_SIGNING_KEY:-}
DARWIN_DIR=${SEATIME_DARWIN_DIR:-}
HOST=$(uname -s)

sum() { # sum FILE: its SHA-256
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

case " $TARGETS" in
  *" darwin/"*)
    if [ "$HOST" != Darwin ] && [ -z "$DARWIN_DIR" ] && [ -n "$KEY" ] && [ -z "${SEATIME_ALLOW_NO_ICON:-}" ]; then
      echo "a signed release needs the macOS binaries from a Mac: set SEATIME_DARWIN_DIR (see the top of this script)" >&2
      exit 1
    fi
    if [ "$HOST" != Darwin ] && [ -n "$DARWIN_DIR" ]; then
      [ -f "$DARWIN_DIR/SHA256SUMS" ] || { echo "$DARWIN_DIR has no SHA256SUMS" >&2; exit 1; }
      got=$(cat "$DARWIN_DIR/VERSION" 2>/dev/null || true)
      [ "$got" = "$VERSION" ] || { echo "$DARWIN_DIR was built as '$got', not $VERSION" >&2; exit 1; }
    fi
    ;;
esac

if [ -n "$KEY" ]; then
  # before building: the key must match internal/client/update/key.go and
  # the version must be a plain vX.Y.Z
  go run ./cmd/seatime-release check -key "$KEY" -version "$VERSION"
fi

rm -rf "$OUT"
mkdir -p "$OUT"
for t in $TARGETS; do
  os=${t%/*}
  arch=${t#*/}
  name="seatime-$os-$arch"
  [ "$os" = windows ] && name="$name.exe"
  cgo=0
  if [ "$os" = darwin ]; then
    if [ "$HOST" = Darwin ]; then
      cgo=1
      # run on every macOS this Go version supports, not only this Mac's
      export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-13.0}"
    elif [ -n "$DARWIN_DIR" ]; then
      want=$(awk -v f="$name" '{n=$2; sub(/^\*/, "", n); if (n == f) print $1}' "$DARWIN_DIR/SHA256SUMS")
      [ -n "$want" ] && [ "$(sum "$DARWIN_DIR/$name")" = "$want" ] || { echo "$DARWIN_DIR/$name does not match its SHA256SUMS" >&2; exit 1; }
      echo "taking $name ($VERSION) from $DARWIN_DIR"
      cp "$DARWIN_DIR/$name" "$OUT/$name"
      chmod 755 "$OUT/$name" # an artifact download drops the mode
      continue
    else
      echo "warning: $name is built here without the menu bar icon (no Mac, no SEATIME_DARWIN_DIR)" >&2
    fi
  fi
  echo "building $name ($VERSION)"
  CGO_ENABLED=$cgo GOOS=$os GOARCH=$arch \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/$name" ./cmd/seatime
  if [ "$cgo" = 0 ] && [ -n "$DARWIN_DIR" ] && [ -f "$DARWIN_DIR/$name" ] && [ -z "${SEATIME_SKIP_REPRO:-}" ]; then
    # the Mac built this one too, from what should be the same source
    cmp -s "$OUT/$name" "$DARWIN_DIR/$name" || {
      echo "$name built here differs from the one in $DARWIN_DIR: the two were not built from the same source and Go version" >&2
      exit 1
    }
    echo "  same bytes as the one in $DARWIN_DIR"
  fi
done
echo "$VERSION" > "$OUT/VERSION"

(
  cd "$OUT"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum seatime-* > SHA256SUMS
  else
    shasum -a 256 seatime-* > SHA256SUMS
  fi
)
echo "wrote $OUT/SHA256SUMS:"
cat "$OUT/SHA256SUMS"

if [ -n "$KEY" ]; then
  go run ./cmd/seatime-release sign -key "$KEY" -version "$VERSION" "$OUT"
else
  echo "unsigned: no SEATIME_SIGNING_KEY, so no $OUT/RELEASE; clients will not update to this build" >&2
fi
