#!/bin/sh
# Xcode "Run Script" phase (Dialler target): keep the symbols of every build
# that goes onto a device, keyed by binary UUID, so a crash or CPU report
# from the phone can be symbolicated later — even after Xcode has rebuilt.
# Copies the app executable, its debug dylib (Debug builds keep symbols in
# the binaries) and the dSYM when one is produced, into ios/dsyms/<UUID>/.
# Silent no-op outside Xcode. See `make symbolicate FILE=…`.
set -eu
[ -n "${TARGET_BUILD_DIR:-}" ] || exit 0
OUT="${SRCROOT:-.}/../dsyms"
APP="$TARGET_BUILD_DIR/${WRAPPER_NAME:-Dialler.app}"
mkdir -p "$OUT"
keep() { # file
  f="$1"; [ -f "$f" ] || return 0
  for u in $(dwarfdump --uuid "$f" 2>/dev/null | awk '{print $2}'); do
    mkdir -p "$OUT/$u" && cp -f "$f" "$OUT/$u/" && echo "symbols: $u ← $(basename "$f")"
  done
}
keep "$APP/${EXECUTABLE_NAME:-Dialler}"
for d in "$APP"/*.debug.dylib; do keep "$d"; done
if [ -n "${DWARF_DSYM_FOLDER_PATH:-}" ] && [ -d "$DWARF_DSYM_FOLDER_PATH/${DWARF_DSYM_FILE_NAME:-}" ]; then
  for u in $(dwarfdump --uuid "$DWARF_DSYM_FOLDER_PATH/$DWARF_DSYM_FILE_NAME" 2>/dev/null | awk '{print $2}'); do
    mkdir -p "$OUT/$u" && cp -Rf "$DWARF_DSYM_FOLDER_PATH/$DWARF_DSYM_FILE_NAME" "$OUT/$u/"
  done
fi
# Keep the newest 20 builds.
ls -dt "$OUT"/*/ 2>/dev/null | tail -n +21 | xargs rm -rf 2>/dev/null || true
exit 0
