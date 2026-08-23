#!/bin/sh
# Mullion installer for macOS:
#   curl -fsSL https://raw.githubusercontent.com/anabiiil/mullion/main/install.sh | sh
# Downloads the right binary for this Mac from the latest GitHub release
# and hands over to `mullion setup`, which installs the full stack.
set -eu

REPO="anabiiil/mullion"

case "$(uname -s)" in
  Darwin) ;;
  *) echo "This installer is for macOS. On Windows, download mullion.exe from:"; \
     echo "  https://github.com/$REPO/releases/latest"; exit 1 ;;
esac

case "$(uname -m)" in
  arm64)  ARCH="arm64" ;;
  x86_64) ARCH="amd64" ;;
  *) echo "Unsupported CPU architecture: $(uname -m)"; exit 1 ;;
esac

echo "Finding the latest release..."
TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
  grep '"tag_name"' | head -1 | cut -d'"' -f4)
if [ -z "$TAG" ]; then
  echo "Could not resolve the latest release — download it manually from:"
  echo "  https://github.com/$REPO/releases/latest"
  exit 1
fi

URL="https://github.com/$REPO/releases/download/$TAG/mullion-$TAG-darwin-$ARCH.tar.gz"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading mullion $TAG ($ARCH)..."
curl -fL --progress-bar "$URL" | tar -xz -C "$TMP"
chmod +x "$TMP/mullion"
xattr -c "$TMP/mullion" 2>/dev/null || true

# Setup copies the binary into ~/.mullion/bin itself and asks a couple of
# questions — reattach stdin to the terminal, since this script usually
# arrives through a pipe.
echo
if [ -r /dev/tty ]; then
  "$TMP/mullion" setup < /dev/tty
else
  "$TMP/mullion" setup
fi
