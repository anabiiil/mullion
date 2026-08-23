#!/bin/sh
# One-time bootstrap for Mullion's package-manager repos:
#   - github.com/anabiiil/homebrew-tap   (brew install anabiiil/tap/mullion)
#   - github.com/anabiiil/scoop-bucket   (scoop bucket add mullion ...)
#
# Run it once from anywhere: sh packaging/bootstrap-distribution.sh
# If the GitHub CLI (gh) is installed and authenticated it creates the
# repos too; otherwise create the two empty public repos on github.com
# first, then run this to push their initial content.
set -eu

OWNER="anabiiil"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

seed() { # seed <repo> <readme-content-file>
  repo="$1"; readme="$2"
  dir="$TMP/$repo"
  mkdir -p "$dir"
  cp "$readme" "$dir/README.md"
  git -C "$dir" init -q -b main
  git -C "$dir" add .
  git -C "$dir" commit -q -m "Bootstrap $repo"
  if command -v gh >/dev/null 2>&1; then
    gh repo create "$OWNER/$repo" --public --source "$dir" --push && return 0
    echo "gh repo create failed (repo may already exist) — pushing directly..."
  fi
  git -C "$dir" remote add origin "https://github.com/$OWNER/$repo.git"
  git -C "$dir" push -u origin main
}

cat > "$TMP/tap-readme.md" << 'EOF'
# Homebrew tap for Mullion

```
brew install anabiiil/tap/mullion
```

`Formula/mullion.rb` is generated automatically by the release workflow
in [anabiiil/mullion](https://github.com/anabiiil/mullion) — do not edit
it by hand.
EOF

cat > "$TMP/bucket-readme.md" << 'EOF'
# Scoop bucket for Mullion

```
scoop bucket add mullion https://github.com/anabiiil/scoop-bucket
scoop install mullion
```

`bucket/mullion.json` is generated automatically by the release workflow
in [anabiiil/mullion](https://github.com/anabiiil/mullion) — do not edit
it by hand.
EOF

echo "==> homebrew-tap"
seed homebrew-tap "$TMP/tap-readme.md"
echo "==> scoop-bucket"
seed scoop-bucket "$TMP/bucket-readme.md"

echo
echo "Done. Now add a fine-grained PAT with Contents:write on BOTH repos as"
echo "two secrets in anabiiil/mullion (Settings > Secrets > Actions):"
echo "  HOMEBREW_TAP_TOKEN   and   SCOOP_BUCKET_TOKEN"
echo "(the same token value works for both), then push a version tag."
