#!/bin/sh
# Copyright (C) 2026 the SeaTime authors. Licensed under the GNU AGPL v3.
# SPDX-License-Identifier: AGPL-3.0-only
# SeaTime installer for macOS and Linux.
#   curl -fsSL @@SERVER@@/install.sh | sh
# Installs a single static binary into ~/.local/bin (no sudo), checks its
# SHA-256 against the server's SHA256SUMS, then runs `seatime setup --yes`.
# Env: SEATIME_INSTALL_DIR (default ~/.local/bin), SEATIME_NO_MODIFY_PATH=1, SEATIME_NO_SETUP=1.

main() {
  set -eu
  SERVER="@@SERVER@@"

  say() { printf '%s\n' "$*"; }
  die() { printf 'seatime install: %s\n' "$*" >&2; exit 1; }

  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*) die "this is a Windows shell; in PowerShell run: irm $SERVER/install.ps1 | iex" ;;
    *) die "unsupported OS $(uname -s) (SeaTime supports macOS, Linux and Windows)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported CPU $(uname -m) (SeaTime supports amd64 and arm64)" ;;
  esac
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
    arch=arm64 # shell is running under Rosetta on an Apple Silicon Mac
  fi
  name="seatime-$os-$arch"

  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
  else
    die "need curl or wget to download seatime"
  fi

  tmp=$(mktemp -d 2>/dev/null || mktemp -d -t seatime) || die "cannot create a temp dir"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "Downloading $name from $SERVER ..."
  fetch "$SERVER/dl/SHA256SUMS" "$tmp/SHA256SUMS" </dev/null || die "could not download $SERVER/dl/SHA256SUMS"
  fetch "$SERVER/dl/$name" "$tmp/$name" </dev/null || die "could not download $SERVER/dl/$name"

  want=$(awk -v f="$name" '{n=$2; sub(/^\*/, "", n); if (n == f) { print tolower($1); exit }}' "$tmp/SHA256SUMS")
  [ -n "$want" ] || die "$name is not listed in SHA256SUMS"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$name" | awk '{print tolower($1)}')
  elif command -v shasum >/dev/null 2>&1; then
    got=$(shasum -a 256 "$tmp/$name" | awk '{print tolower($1)}')
  elif command -v openssl >/dev/null 2>&1; then
    got=$(openssl dgst -sha256 "$tmp/$name" | awk '{print tolower($NF)}')
  else
    die "no sha256sum, shasum or openssl found to verify the download"
  fi
  [ "$got" = "$want" ] || die "checksum mismatch for $name (expected $want, got $got); refusing to install"
  say "Checksum OK."

  dir=${SEATIME_INSTALL_DIR:-$HOME/.local/bin}
  mkdir -p "$dir" || die "cannot create $dir"
  chmod 755 "$tmp/$name"
  # Copy then rename: replacing a running binary in place is safe on Unix.
  cp "$tmp/$name" "$dir/seatime.new" || die "cannot write to $dir"
  mv -f "$dir/seatime.new" "$dir/seatime" || die "cannot install to $dir/seatime"
  say "Installed $dir/seatime"

  case ":$PATH:" in
    *":$dir:"*) ;;
    *)
      if [ -z "${SEATIME_NO_MODIFY_PATH:-}" ]; then
        case "${SHELL:-}" in
          */zsh)  rc="$HOME/.zshrc" ;;
          */bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
          *)      rc="$HOME/.profile" ;;
        esac
        if ! grep -qs "# added by seatime" "$rc" 2>/dev/null; then
          printf '\nexport PATH="%s:$PATH" # added by seatime\n' "$dir" >> "$rc" 2>/dev/null &&
            say "Added $dir to PATH in $rc (open a new terminal to use \`seatime\`)."
        fi
      else
        say "Note: $dir is not on your PATH."
      fi
      ;;
  esac

  if [ -n "${SEATIME_NO_SETUP:-}" ]; then
    say "Skipping setup (SEATIME_NO_SETUP). Run: $dir/seatime setup --yes --server $SERVER"
    return 0
  fi
  # </dev/null: when piped from curl, stdin is this script; keep children off it.
  "$dir/seatime" setup --yes --server "$SERVER" </dev/null
}

main "$@"
