#!/bin/sh
# Install the mcplogview release binary from GitHub (Linux and macOS, amd64 and arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/skromesch/mcplogview-go/main/install.sh | sh
#
# Options (flags or environment variables):
#   -v, --version VERSION   release to install, e.g. v0.1.0        (MCPLOGVIEW_VERSION, default: latest)
#   -d, --dir DIR           where to put the binary                (MCPLOGVIEW_INSTALL_DIR)
#                           default: /usr/local/bin when run as root, else ~/.local/bin
#   -h, --help
# Piped into sh, pass flags with:  curl -fsSL .../install.sh | sh -s -- --version v0.1.0
#
# The archive's SHA-256 is checked against the release's checksums.txt before anything is installed.
set -eu

REPO="skromesch/mcplogview-go"
BIN="mcplogview"
BASE_URL="${MCPLOGVIEW_BASE_URL:-https://github.com/$REPO/releases}" # overridable for testing
version="${MCPLOGVIEW_VERSION:-latest}"
dir="${MCPLOGVIEW_INSTALL_DIR:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Install the mcplogview release binary from GitHub (Linux and macOS, amd64 and arm64).

  -v, --version VERSION   release to install, e.g. v0.1.0   (MCPLOGVIEW_VERSION, default: latest)
  -d, --dir DIR           where to put the binary           (MCPLOGVIEW_INSTALL_DIR)
                          default: /usr/local/bin as root, else ~/.local/bin
  -h, --help

Piped into sh:  curl -fsSL <url>/install.sh | sh -s -- --version v0.1.0
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    -v|--version) [ $# -ge 2 ] || die "$1 needs a value"; version="$2"; shift 2 ;;
    -d|--dir)     [ $# -ge 2 ] || die "$1 needs a value"; dir="$2"; shift 2 ;;
    -h|--help)    usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

case "$(uname -s)" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $(uname -s). Windows users: download the .zip from https://github.com/$REPO/releases" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

if [ -z "$dir" ]; then
  if [ "$(id -u)" = 0 ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q "$1" -O "$2"; }
else
  die "need curl or wget"
fi
command -v tar >/dev/null 2>&1 || die "need tar"
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum to verify the download"
fi

if [ "$version" = latest ]; then
  url="$BASE_URL/latest/download"
else
  case "$version" in v*) ;; *) version="v$version" ;; esac
  url="$BASE_URL/download/$version"
fi

archive="${BIN}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $archive ($version)"
fetch "$url/$archive" "$tmp/$archive" || die "download failed: $url/$archive"
fetch "$url/checksums.txt" "$tmp/checksums.txt" || die "download failed: $url/checksums.txt"

want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$want" = "$got" ] || die "checksum mismatch for $archive (expected $want, got $got)"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN" || die "could not extract $BIN from $archive"
mkdir -p "$dir" || die "cannot create $dir (use --dir, or run with sudo)"
[ -w "$dir" ] || die "$dir is not writable (use --dir, or run with sudo)"
# Write next to the target, then rename: replacing a running binary in place is safe.
cp "$tmp/$BIN" "$dir/.$BIN.new" && chmod 755 "$dir/.$BIN.new" && mv -f "$dir/.$BIN.new" "$dir/$BIN"

say "Installed $dir/$BIN"
"$dir/$BIN" version 2>/dev/null || true
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say ""; say "$dir is not on your PATH. Add it, e.g.:"; say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
say ""
say "Next: run 'mcplogview' for help, or 'mcplogview configure' to set up your log sources."
