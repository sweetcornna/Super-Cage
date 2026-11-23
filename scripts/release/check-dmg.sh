#!/usr/bin/env bash
# What the in-app updater of every published Super Cage requires of a macOS
# release image (desktop/Updates.swift): it mounts the DMG and accepts only
# "Super Cage.app" with this bundle id, executable and team, at most 50000
# entries and 1 GiB, nothing that resolves outside the App, and signed nested
# helpers. Prints one line per check; exits 1 if any check fails.
#
#   scripts/release/check-dmg.sh <dmg> <version> <team-id> [--not-notarized]
set -uo pipefail
if [[ $# -lt 3 || $# -gt 4 || ( $# == 4 && $4 != --not-notarized ) ]]; then
  echo 'Usage: scripts/release/check-dmg.sh <dmg> <version> <team-id> [--not-notarized]' >&2
  exit 2
fi
dmg="$1" version="$2" team="$3" notarized=1
if [[ $# == 4 ]]; then notarized=0; fi
[[ "$team" =~ ^[A-Z0-9]{10}$ ]] || { echo "Not an Apple team id: $team" >&2; exit 2; }
bundle_id=io.github.lee-xeo.claude-cage
failures=0
result() { # <0|1> <description>
  if [[ $1 == 0 ]]; then printf 'ok      %s\n' "$2"; else printf 'FAIL    %s\n' "$2"; failures=$((failures + 1)); fi
}
check() { # <description> <command...>
  local description="$1"; shift
  "$@" > /dev/null 2>&1
  result $? "$description"
}

mount="$(mktemp -d "${TMPDIR:-/tmp}/super-cage-dmg.XXXXXX")"
# hdiutil attach still works on macOS 27 but warns that it is deprecated; keep other errors.
if ! hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$mount" "$dmg" 2> >(grep -v 'is deprecated' >&2) > /dev/null; then
  rmdir "$mount"; echo 'FAIL    the image does not mount'; exit 1
fi
trap 'hdiutil detach "$mount" > /dev/null 2>&1 || hdiutil detach -force "$mount" > /dev/null 2>&1; rmdir "$mount" 2> /dev/null' EXIT
app="$mount/Super Cage.app"
check 'the image holds Super Cage.app' test -d "$app" -a ! -L "$app"
plist="$app/Contents/Info.plist"
value() { /usr/libexec/PlistBuddy -c "Print :$1" "$plist" 2> /dev/null; }
id="$(value CFBundleIdentifier)" cage_version="$(value CageVersion)" executable="$(value CFBundleExecutable)"
check "bundle id $id" test "$id" = "$bundle_id"
check "CageVersion $cage_version" test "$cage_version" = "$version"
check "executable $executable" test "$executable" = Cage
check "minimum macOS $(value LSMinimumSystemVersion)" test "$(value LSMinimumSystemVersion)" = 14.0
entries="$(find "$app" | wc -l | tr -d ' ')"
kib="$(du -sk "$app" | cut -f1)"
check "$entries entries (limit 50000)" test "$entries" -le 50000
check "$((kib / 1024)) MiB unpacked (limit 1024)" test "$kib" -le $((1024 * 1024))
check 'no entry resolves outside the App' python3 -I -c '
import os, sys
app = os.path.realpath(sys.argv[1])
for root, dirs, files in os.walk(sys.argv[1]):
    for name in dirs + files:
        if not (os.path.realpath(os.path.join(root, name)) + "/").startswith(app + "/"):
            sys.exit(1)
' "$app"
requirement='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists'
check "App signed by team $team as $bundle_id" codesign --verify --strict --all-architectures --deep \
  -R="$requirement and identifier \"$bundle_id\" and certificate leaf[subject.OU] = \"$team\"" "$app"
check 'arm64 executable' lipo "$app/Contents/MacOS/Cage" -verify_arch arm64
for helper in cage cage-attach mihomo CageUpdateInstaller; do
  check "Resources/$helper present and signed by the team" codesign --verify --strict --all-architectures \
    -R="$requirement and certificate leaf[subject.OU] = \"$team\"" "$app/Contents/Resources/$helper"
done
if [[ $notarized == 1 ]]; then
  check 'Gatekeeper accepts the App' spctl --assess --type execute "$app"
else
  printf 'SKIPPED Gatekeeper (not notarized)\n'
fi
exit $((failures > 0))
