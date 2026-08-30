#!/bin/sh
# Remove the LaunchAgent and the files install.sh created.
set -eu
LABEL=io.local.ninelives
BIN="${BIN:-$HOME/bin/ninelives}"
OUT="${OUT:-$HOME/.config/runcat-neo-metrics/claude.json}"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$PLIST" "$BIN" "$OUT"
echo "Removed the LaunchAgent, $BIN and $OUT."
echo "Remove the card from RunCat Neo's settings by hand."
