#!/usr/bin/env bash
# Install the App from a release DMG as the only copy, after moving the
# installed one into a .noindex backup folder (Spotlight then never offers two).
#
#   scripts/release/install-app.sh <dmg> <backup-dir>
#
# CAGE_INSTALL_APP overrides the destination (default /Applications/Super Cage.app).
set -uo pipefail
dmg="${1:?Usage: scripts/release/install-app.sh <dmg> <backup-dir>}"
backups="${2:?Usage: scripts/release/install-app.sh <dmg> <backup-dir>}"
app="${CAGE_INSTALL_APP:-/Applications/Super Cage.app}"
[[ "$backups" == *.noindex ]] || { echo "The backup folder must end in .noindex: $backups" >&2; exit 2; }
dest=''
if [[ -e "$app" ]]; then
  old="$(/usr/libexec/PlistBuddy -c 'Print :CageVersion' "$app/Contents/Info.plist" 2> /dev/null || echo unknown)"
  dest="$backups/Super-Cage-$old.app"
  [[ ! -e "$dest" ]] || { echo "The backup already exists: $dest" >&2; exit 2; }
  if pgrep -f -q "^$app/Contents/"; then echo 'Super Cage is running; quit it first.' >&2; exit 3; fi
fi
mount="$(mktemp -d "${TMPDIR:-/tmp}/super-cage-install.XXXXXX")"
hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$mount" "$dmg" 2> >(grep -v 'is deprecated' >&2) > /dev/null || { rmdir "$mount"; echo 'The image does not mount.' >&2; exit 4; }
trap 'hdiutil detach "$mount" > /dev/null 2>&1 || hdiutil detach -force "$mount" > /dev/null 2>&1; rmdir "$mount" 2> /dev/null' EXIT
codesign --verify --deep --strict "$mount/Super Cage.app" || { echo 'The App in the image does not verify.' >&2; exit 5; }
if [[ -n "$dest" ]]; then
  mkdir -p "$backups"
  mv "$app" "$dest" || { echo 'Could not move the installed App.' >&2; exit 6; }
  echo "Moved $old to $dest"
fi
if ! ditto "$mount/Super Cage.app" "$app"; then
  echo 'Copy failed; restoring the previous App.' >&2
  rm -rf "$app"
  if [[ -n "$dest" ]]; then mv "$dest" "$app"; fi
  exit 7
fi
status=0
verify() { # <description> <command...>
  local description="$1"; shift
  if "$@" > /dev/null 2>&1; then echo "$description: ok"; else echo "$description: FAIL"; status=1; fi
}
verify signature codesign --verify --deep --strict "$app"
verify 'notarization ticket' xcrun stapler validate "$app"
verify Gatekeeper spctl --assess --type execute "$app"
echo "Installed $(/usr/libexec/PlistBuddy -c 'Print :CageVersion' "$app/Contents/Info.plist") at $app"
exit "$status"
