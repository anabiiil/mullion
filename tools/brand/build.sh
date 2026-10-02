#!/usr/bin/env bash
# Regenerates every raster brand asset:
#   internal/ui/favicon.png         panel favicon + Windows app-window icon (256px)
#   assets/brand/app-icon-1024.png  macOS app icon source (macapp/build.sh -> AppIcon.icns)
#   mullion.ico                     Windows icon, 16-256px
#   rsrc_windows_amd64.syso         the exe's resources: icons, plus the version
#                                   info and manifest from versioninfo.json
# The Windows icons and the favicon come from winicon.go (pure Go, runs on
# any OS); the macOS app icon needs render.swift (AppKit), so it is only
# redrawn on a Mac. Runnable from any cwd.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if command -v swift >/dev/null 2>&1; then
  swift "$SCRIPT_DIR/render.swift" "$REPO_ROOT"
else
  echo "swift not found: keeping the existing assets/brand/app-icon-1024.png"
fi
cd "$REPO_ROOT"
go run ./tools/brand/winicon.go "$TMP_DIR/ico" "$REPO_ROOT"
# Rebuild the .syso from versioninfo.json first so its version strings
# can't go stale (the 2.0.0 exe still said 1.1.0), then icons.go swaps the
# new frames into it and rewrites mullion.ico.
go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.7.0 \
  -64 -o rsrc_windows_amd64.syso versioninfo.json
go run ./tools/brand/icons.go "$TMP_DIR/ico" "$REPO_ROOT"
