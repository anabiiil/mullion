#!/usr/bin/env bash
# Regenerates every raster brand asset from the SVG sources in assets/brand:
#   internal/ui/favicon.png         panel favicon + Windows app-window icon (256px)
#   assets/brand/app-icon-1024.png  macOS app icon source (macapp/build.sh -> AppIcon.icns)
#   mullion.ico                     Windows icon, 16-256px
#   rsrc_windows_amd64.syso         the exe's resources, icons swapped in place
# macOS only (render.swift uses AppKit to rasterize the SVGs); needs just the
# command line tools and Go. Runnable from any cwd.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

swift "$SCRIPT_DIR/render.swift" "$REPO_ROOT" "$TMP_DIR/ico"
(cd "$REPO_ROOT" && go run ./tools/brand/icons.go "$TMP_DIR/ico" "$REPO_ROOT")
