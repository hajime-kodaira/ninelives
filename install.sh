#!/bin/sh
# Build ninelives, install it, and register a LaunchAgent that refreshes the
# RunCat Neo metrics file every 5 minutes.
#
#   ./install.sh              # 57% style
#   LIVES=1 ./install.sh      # 6/9 style
#   BIN=/usr/local/bin/ninelives ./install.sh
set -eu

LABEL=io.local.ninelives
BIN="${BIN:-$HOME/bin/ninelives}"
OUT="${OUT:-$HOME/.config/runcat-neo-metrics/claude.json}"
LOG="${LOG:-$HOME/Library/Logs/ninelives.log}"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
SRC_DIR=$(cd "$(dirname "$0")" && pwd)

[ "$(uname -s)" = "Darwin" ] || { echo "install.sh only supports macOS" >&2; exit 1; }
command -v go >/dev/null || { echo "Go is required: https://go.dev/dl/" >&2; exit 1; }

echo "==> building $BIN"
mkdir -p "$(dirname "$BIN")"
(cd "$SRC_DIR" && go build -o "$BIN" .)

echo "==> first run -> $OUT"
if [ -n "${LIVES:-}" ]; then
	"$BIN" -out "$OUT" -lives
	EXTRA='<string>-lives</string>'
else
	"$BIN" -out "$OUT"
	EXTRA=''
fi

echo "==> writing $PLIST"
mkdir -p "$(dirname "$PLIST")" "$(dirname "$LOG")"
sed -e "s|__BIN__|$BIN|" \
    -e "s|__OUT__|$OUT|" \
    -e "s|__LOG__|$LOG|" \
    -e "s|__EXTRA_ARGS__|$EXTRA|" \
    "$SRC_DIR/$LABEL.plist" > "$PLIST"
plutil -lint "$PLIST" >/dev/null

echo "==> (re)loading the LaunchAgent"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"

cat <<MSG

Done. The metrics file is at:
  $OUT

Register it in RunCat Neo: Settings > Metrics > Custom Metrics > +
Hidden folders do not show in the file dialog, so press Cmd+Shift+G and paste
the path above.

Errors from the scheduled runs land in $LOG
MSG
