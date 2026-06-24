#!/usr/bin/env bash
# Frostgate / Frostcode installer (macOS / Linux / Git Bash).
# Builds both binaries into ~/.local/bin (or $PREFIX), copies config.json to
# ~/.frostcode/config.json, and prints the PATH/env lines to add to your shell.
#
# Usage (from the repo root):
#   ./install.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")" && pwd)"
BIN="${PREFIX:-$HOME/.local/bin}"
CFGDIR="$HOME/.frostcode"
CFG="$CFGDIR/config.json"

# On Windows (Git Bash / MSYS / Cygwin) the binaries MUST end in .exe, otherwise
# Windows doesn't treat them as executable and pops an "Open with" dialog when
# you try to run them. Detect the host and pick the right suffix.
EXT=""
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN* | Windows_NT) EXT=".exe" ;;
esac

mkdir -p "$BIN" "$CFGDIR"

echo "Building binaries -> $BIN"
( cd "$REPO" && go build -o "$BIN/frostgate$EXT" ./cmd/frostgate )
( cd "$REPO" && go build -o "$BIN/frostcode$EXT" ./cmd/frostcode )

if [ -f "$REPO/config.json" ] && [ ! -f "$CFG" ]; then
  cp "$REPO/config.json" "$CFG"
  echo "Copied config.json -> $CFG"
else
  echo "Keeping existing $CFG (or none to copy)"
fi

echo ""
echo "Add these to your shell profile (~/.bashrc, ~/.zshrc) if not already present:"
echo "  export PATH=\"$BIN:\$PATH\""
echo "  export FROSTCODE_CONFIG=\"$CFG\""
echo ""
echo "Then: frostcode   (agent)   |   frostgate   (gateway + dashboard)"
