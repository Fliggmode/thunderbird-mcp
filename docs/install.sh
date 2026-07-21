#!/bin/sh
# Installer for thunderbird-mcp (https://github.com/mudler/thunderbird-mcp).
#
#   curl -fsSL https://mudler.github.io/thunderbird-mcp/install.sh | sh
#
# Fallback URL (same script, served from the repo):
#   curl -fsSL https://raw.githubusercontent.com/mudler/thunderbird-mcp/master/docs/install.sh | sh
#
# Environment:
#   VERSION  release tag to install (e.g. v0.1.0); defaults to the latest release
#   BINDIR   install directory; defaults to ~/.local/bin
set -eu

REPO="mudler/thunderbird-mcp"
BIN="thunderbird-mcp"
BINDIR="${BINDIR:-$HOME/.local/bin}"

err() { printf 'install: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*" >&2; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS '$(uname -s)'. Windows and other platforms: download an archive from https://github.com/$REPO/releases" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) err "unsupported architecture '$(uname -m)' (need x86_64 or arm64)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  err "need curl or wget"
fi

tag="${VERSION:-}"
if [ -z "$tag" ]; then
  api="https://api.github.com/repos/$REPO/releases/latest"
  tmpjson=$(mktemp)
  fetch "$api" "$tmpjson" || { rm -f "$tmpjson"; err "cannot reach $api"; }
  tag=$(grep -m1 '"tag_name":' "$tmpjson" | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  rm -f "$tmpjson"
  [ -n "$tag" ] || err "could not determine the latest release tag; set VERSION=vX.Y.Z and retry"
fi

# GoReleaser asset names drop the leading "v" of the tag.
version=${tag#v}
archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT INT TERM

info "Downloading $BIN $tag ($os/$arch)..."
fetch "$base/$archive" "$workdir/$archive" || err "download failed: $base/$archive"
fetch "$base/checksums.txt" "$workdir/checksums.txt" || err "download failed: $base/checksums.txt"

want=$(grep " $archive\$" "$workdir/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || err "$archive not found in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$workdir/$archive" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$workdir/$archive" | cut -d' ' -f1)
fi
[ "$got" = "$want" ] || err "checksum mismatch for $archive (expected $want, got $got)"

tar -xzf "$workdir/$archive" -C "$workdir" "$BIN"

mkdir -p "$BINDIR" 2>/dev/null || true
if [ -d "$BINDIR" ] && [ -w "$BINDIR" ]; then
  install -m 0755 "$workdir/$BIN" "$BINDIR/$BIN"
else
  info "$BINDIR is not writable; using sudo."
  sudo mkdir -p "$BINDIR"
  sudo install -m 0755 "$workdir/$BIN" "$BINDIR/$BIN"
fi

info "Installed $BIN $tag to $BINDIR/$BIN"

case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) info "NOTE: $BINDIR is not on your PATH. Add it, e.g.: export PATH=\"$BINDIR:\$PATH\"" ;;
esac

info ""
info "Quick start (profile is auto-discovered; override with THUNDERBIRD_PROFILE):"
info "  THUNDERBIRD_PROFILE=~/.thunderbird/xxxx.default-release $BIN"
