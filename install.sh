#!/bin/sh
# Installs the latest saba release binary.
#   curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sh
# Environment:
#   SABA_INSTALL_DIR   target directory (default: /usr/local/bin, or ~/.local/bin without root)
#   SABA_VERSION       release tag to install (default: latest)
set -eu

repo="egoist/saba"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux | darwin) ;;
  *) echo "saba: unsupported operating system: $os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "saba: unsupported architecture: $arch" >&2; exit 1 ;;
esac

version="${SABA_VERSION:-latest}"
if [ "$version" = "latest" ]; then
  url="${SABA_DOWNLOAD_BASE:-https://github.com/$repo/releases/latest/download}/saba-$os-$arch"
else
  url="${SABA_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/$version}/saba-$os-$arch"
fi

dir="${SABA_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ "$(id -u)" = "0" ] || [ -w /usr/local/bin ]; then
    dir=/usr/local/bin
  else
    dir="$HOME/.local/bin"
  fi
fi
mkdir -p "$dir"

# Download next to the destination so the final rename is atomic.
tmp="$dir/.saba-download-$$"
echo "Downloading $url"
if ! curl -fsSL "$url" -o "$tmp"; then
  echo "saba: download failed. Check https://github.com/$repo/releases for available builds." >&2
  echo "saba: a partial download may remain at $tmp" >&2
  exit 1
fi
chmod 755 "$tmp"
mv -f "$tmp" "$dir/saba"

echo "Installed $("$dir/saba" --version) to $dir/saba"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Note: $dir is not on your PATH." ;;
esac
echo "Next: saba server setup <user@host>"
