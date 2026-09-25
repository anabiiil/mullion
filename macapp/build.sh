#!/usr/bin/env bash
# Builds Mullion.app (a plain AppKit/WKWebView shell, see Mullion.swift) as a
# universal binary and packages it into internal/macapp/bundle/Mullion.app.tar.gz,
# which the Go side embeds and installs into /Applications. Runnable from any
# cwd; no Xcode project involved, just swiftc + the macOS command line tools.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# VERSION can be passed in (the release workflow derives it from the git
# tag); otherwise fall back to the Go module's own version constant so a
# local `bash macapp/build.sh` produces a sensibly-versioned app too.
if [ -z "${VERSION:-}" ]; then
	VERSION="$(sed -n 's/^const Number = "\(.*\)"$/\1/p' "$REPO_ROOT/internal/version/version.go" || true)"
	VERSION="${VERSION:-dev}"
fi

BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT

APP_DIR="$BUILD_DIR/Mullion.app"
CONTENTS_DIR="$APP_DIR/Contents"
mkdir -p "$CONTENTS_DIR/MacOS" "$CONTENTS_DIR/Resources"

echo "Building Mullion.app $VERSION..."

# Universal binary: compile each arch separately (swiftc doesn't take
# multiple -target flags at once) and glue them together with lipo.
# -runtime-compatibility-version none: without it, targeting macos12 pulls
# in Swift's pre-5.6 concurrency back-deployment shims, whose x86_64 slice
# newer Command Line Tools releases no longer ship (arm64/arm64e only),
# which fails the x86_64 link with "symbol(s) not found". We don't rely on
# any back-deployed runtime behavior, so disabling the compatibility shim
# entirely is safe.
swiftc -runtime-compatibility-version none -O -target arm64-apple-macos12 \
	-framework AppKit -framework WebKit \
	-o "$BUILD_DIR/Mullion-arm64" "$SCRIPT_DIR/Mullion.swift"
swiftc -runtime-compatibility-version none -O -target x86_64-apple-macos12 \
	-framework AppKit -framework WebKit \
	-o "$BUILD_DIR/Mullion-x86_64" "$SCRIPT_DIR/Mullion.swift"
lipo -create -output "$CONTENTS_DIR/MacOS/Mullion" \
	"$BUILD_DIR/Mullion-arm64" "$BUILD_DIR/Mullion-x86_64"
chmod +x "$CONTENTS_DIR/MacOS/Mullion"

sed "s/__VERSION__/$VERSION/g" "$SCRIPT_DIR/Info.plist" > "$CONTENTS_DIR/Info.plist"

# AppIcon.icns from the 1024x1024 brand source (the coral mark on a
# midnight tile, drawn on Apple's icon grid — regenerate it with
# tools/brand/build.sh): build the full iconset iconutil expects (each size
# plus its @2x, up through 512@2x) by downscaling it with sips.
ICONSET_DIR="$BUILD_DIR/AppIcon.iconset"
mkdir -p "$ICONSET_DIR"
SOURCE_ICON="$REPO_ROOT/assets/brand/app-icon-1024.png"
for spec in "16:icon_16x16.png" "32:icon_16x16@2x.png" "32:icon_32x32.png" "64:icon_32x32@2x.png" \
	"128:icon_128x128.png" "256:icon_128x128@2x.png" "256:icon_256x256.png" "512:icon_256x256@2x.png" \
	"512:icon_512x512.png" "1024:icon_512x512@2x.png"; do
	size="${spec%%:*}"
	name="${spec#*:}"
	sips -z "$size" "$size" "$SOURCE_ICON" --out "$ICONSET_DIR/$name" >/dev/null
done
iconutil -c icns "$ICONSET_DIR" -o "$CONTENTS_DIR/Resources/AppIcon.icns"

# Ad-hoc sign so Gatekeeper doesn't flag it as entirely unsigned; users still
# need to right-click > Open the first time since it's not notarized.
codesign --force --deep -s - "$APP_DIR"

OUT_DIR="$REPO_ROOT/internal/macapp/bundle"
mkdir -p "$OUT_DIR"
OUT_FILE="$OUT_DIR/Mullion.app.tar.gz"
# COPYFILE_DISABLE: keep macOS tar from adding ._* AppleDouble files,
# which would break the bundle's code-signature seal once extracted.
COPYFILE_DISABLE=1 tar -czf "$OUT_FILE" -C "$BUILD_DIR" Mullion.app

echo "Built $OUT_FILE"
