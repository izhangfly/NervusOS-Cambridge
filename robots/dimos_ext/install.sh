#!/usr/bin/env bash
# Symlink out-of-tree DimOS extensions into a dimos checkout so its adapter
# registry discovers them. Symlink (not copy) so edits here take effect at once
# and a `git pull` in dimos cannot silently clobber our work.
set -euo pipefail

DIMOS_ROOT="${1:-$HOME/dimos}"
EXT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="$DIMOS_ROOT/dimos/hardware/manipulators/nero"

if [ ! -d "$DIMOS_ROOT/dimos/hardware/manipulators" ]; then
  echo "error: no manipulators dir under $DIMOS_ROOT" >&2
  echo "usage: $0 [path-to-dimos-checkout]" >&2
  exit 1
fi

if [ -L "$TARGET" ]; then
  rm "$TARGET"
elif [ -e "$TARGET" ]; then
  echo "error: $TARGET exists and is not a symlink — refusing to overwrite" >&2
  exit 1
fi

ln -s "$EXT_DIR/nero" "$TARGET"
echo "linked $TARGET -> $EXT_DIR/nero"
echo "verify with:  cd $DIMOS_ROOT && ./.venv/bin/python -c \\"
echo "  'from dimos.hardware.manipulators.registry import *; print(\"nero registered\")'"
