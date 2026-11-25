#!/bin/sh
# Cross-build SeaTime for the six supported targets into ./dist and write
# dist/SHA256SUMS. With SEATIME_SIGNING_KEY set it also writes dist/RELEASE,
# the signed manifest that `seatime update` and the daemon's daily check
# install from; without it the build is unsigned and no client updates to it.
#
#   SEATIME_SIGNING_KEY=~/seatime-release.key VERSION=v0.1.0 scripts/release.sh
#   scripts/release.sh                 # unsigned, version from `git describe`
#   TARGETS="linux/amd64 windows/386" scripts/release.sh   # subset
#
# Host everything in dist/ in one folder: the server's SEATIME_DL_DIR, or
# anywhere public with seatimed's SEATIME_DL_UPSTREAM pointing at it.
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
TARGETS=${TARGETS:-"windows/amd64 windows/386 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"}
OUT=${OUT:-dist}
KEY=${SEATIME_SIGNING_KEY:-}

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
  echo "building $name ($VERSION)"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/$name" ./cmd/seatime
done

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
