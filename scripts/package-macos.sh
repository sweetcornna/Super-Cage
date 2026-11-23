#!/usr/bin/env bash
# Build, notarize, and package a Developer ID distribution. No installation.
#
#   scripts/package-macos.sh [--from-built-app | --from-notarized-app] [--no-notarize]
#
# Notarization uses a notarytool keychain profile (CAGE_NOTARY_PROFILE, optionally
# in CAGE_NOTARY_KEYCHAIN) or an App Store Connect API key (CAGE_NOTARY_KEY, the
# .p8 file, with CAGE_NOTARY_KEY_ID and CAGE_NOTARY_ISSUER); nothing is written
# to a keychain for the key. --no-notarize skips every Apple submission and
# Gatekeeper check: the result is for testing, never for publication.
# The release driver (scripts/release/release.sh) packages an App built in a
# clean checkout: CAGE_SOURCE_ROOT names that checkout (its VERSION, Mihomo pin
# and guest-source tool), CAGE_BUILD_OUTPUT the App, as for build-app.sh, and
# CAGE_RELEASE_OUTPUT the output directory.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  echo 'The supported release target is Apple Silicon macOS 14+.' >&2; exit 1
fi
usage='Usage: scripts/package-macos.sh [--from-built-app|--from-notarized-app] [--no-notarize]'
from=source notarize=1
for argument in "$@"; do
  case "$argument" in
    --from-built-app) from=built ;;
    --from-notarized-app) from=notarized ;;
    --no-notarize) notarize=0 ;;
    *) echo "$usage" >&2; exit 2 ;;
  esac
done
: "${CAGE_SIGNING_IDENTITY:?Set CAGE_SIGNING_IDENTITY to your Developer ID Application identity.}"
notary_credentials=()
if [[ $notarize == 0 ]]; then
  :
elif [[ -n "${CAGE_NOTARY_PROFILE:-}" ]]; then
  notary_credentials=(--keychain-profile "$CAGE_NOTARY_PROFILE")
  if [[ -n "${CAGE_NOTARY_KEYCHAIN:-}" ]]; then
    notary_credentials+=(--keychain "$CAGE_NOTARY_KEYCHAIN")
  fi
elif [[ -n "${CAGE_NOTARY_KEY:-}" && -n "${CAGE_NOTARY_KEY_ID:-}" && -n "${CAGE_NOTARY_ISSUER:-}" ]]; then
  if [[ ! -f "$CAGE_NOTARY_KEY" ]]; then echo 'CAGE_NOTARY_KEY must name the App Store Connect API key file (.p8).' >&2; exit 1; fi
  notary_credentials=(--key "$CAGE_NOTARY_KEY" --key-id "$CAGE_NOTARY_KEY_ID" --issuer "$CAGE_NOTARY_ISSUER")
else
  echo 'Set CAGE_NOTARY_PROFILE to a notarytool keychain profile, or CAGE_NOTARY_KEY, CAGE_NOTARY_KEY_ID and CAGE_NOTARY_ISSUER to an App Store Connect API key.' >&2
  exit 1
fi
if [[ "$CAGE_SIGNING_IDENTITY" == - ]]; then echo 'Release packages require a Developer ID identity.' >&2; exit 1; fi
source_root="$(cd "${CAGE_SOURCE_ROOT:-$PWD}" && pwd -P)"
version="$(tr -d '\n\r' < "$source_root/VERSION")"
release="${CAGE_RELEASE_OUTPUT:-$source_root/_output/releases/$version}"
bundle="${CAGE_BUILD_OUTPUT:-$source_root/_output/Super Cage.app}"
mkdir -p "$release"

# Submit and wait. On a rejection, keep Apple's log beside the result.
notarize() {
  local result="$2" status
  xcrun notarytool submit "$1" "${notary_credentials[@]}" --wait --timeout 20m \
    --output-format json > "$result" || true
  if node -e 'const r=JSON.parse(require("fs").readFileSync(process.argv[1])); if(r.status!=="Accepted") { console.error("Apple notarization: " + r.status + "; submission " + r.id); process.exit(1) }' "$result"; then
    return 0
  else
    status=$?
  fi
  local id
  id="$(node -e 'try { console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).id || "") } catch {}' "$result")"
  if [[ -n "$id" ]] && xcrun notarytool log "$id" "${notary_credentials[@]}" "$result.log.json" > /dev/null 2>&1; then
    echo "Apple's notarization log: $result.log.json" >&2
  fi
  return "$status"
}

if [[ $from == source ]]; then
  (cd "$source_root" && bash scripts/build-app.sh)
fi
if [[ $from != notarized && $notarize == 1 ]]; then
  ditto -c -k --keepParent "$bundle" "$release/Cage-notarization.zip"
  notarize "$release/Cage-notarization.zip" "$release/app-notarization.json"
  rm -f "$release/Cage-notarization.zip"
  xcrun stapler staple "$bundle"
fi
actual_version="$(/usr/libexec/PlistBuddy -c 'Print :CageVersion' "$bundle/Contents/Info.plist")"
if [[ "$actual_version" != "$version" ]]; then echo 'App version differs from VERSION; rebuild before packaging.' >&2; exit 1; fi
codesign --verify --deep --strict --verbose=2 "$bundle"
if [[ $notarize == 1 ]]; then
  xcrun stapler validate "$bundle"
  spctl --assess --type execute --verbose=2 "$bundle"
fi
guest_source="cage-macos-guest-$version-source.tar.gz"
guest_materials="$bundle/Contents/Resources/guest-os/sources/$guest_source"
node "$source_root/scripts/guest-os/collect-macos-sshfs-sources.mjs" verify "$guest_materials" \
  "$bundle/Contents/Resources/guest-os/project/cage-sshfs" "$version"
# Copy only the source frozen by this App build, including the final signed
# helper hash. Packaging must not substitute newer Homebrew source material.
cp "$guest_materials" "$release/$guest_source"

# The stage holds a copy of the App: keep it inside the output directory, which
# the release driver puts in a .noindex folder so Spotlight never lists it.
stage="$(mktemp -d "$release/dmg-stage.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
ditto "$bundle" "$stage/Super Cage.app"
ln -s /Applications "$stage/Applications"
cat > "$stage/安装说明 - Read Me.txt" <<'EOF'
Super Cage — 本机隔离编码工作区

1. 将 Super Cage.app 拖入 Applications（应用程序）。
2. 从应用程序打开 Super Cage，设置本地账户，并按首次引导选择客机系统、存储位置和代理。
3. 添加自己的 Clash/Mihomo YAML 订阅或配置并选择节点，创建和启动环境。首次需下载系统镜像和工具，请保持网络连接。
4. 打开项目或新建终端，输入 claude，并在 Super Cage 浏览器内登录自己的 Claude 账户。

需要 Apple Silicon Mac、macOS 14 或更新版本。无需安装 Go、Node.js 或 Clash。
代理服务与 Claude 账户需自行准备。具体版本与发布状态以发行说明为准。
共享项目中的文件修改会直接写回 Mac；请备份重要项目。
升级前请退出 Super Cage 并停止环境，再替换应用程序；配置与项目文件会保留。
已安装旧版 Cage.app 时，先退出旧版，再将它移到废纸篓，只保留 Super Cage.app。

Drag Super Cage.app into Applications, then open it from Applications.
Complete the local account and environment setup, add your own Clash/Mihomo
YAML subscription or configuration, select a node, and start the environment.
Initial setup downloads the selected guest system and developer tools.
Requires Apple Silicon and macOS 14+. Your own proxy and Claude account are required.
If an older Cage.app is installed, quit it and move it to the Trash.
Settings and projects are kept.

入门 / Getting started:
https://github.com/sweetcornna/Super-Cage/blob/main/docs/getting-started.md
问题反馈 / Issues:
https://github.com/sweetcornna/Super-Cage/issues
EOF

artifact="Super-Cage-$version-macos-arm64.dmg"
hdiutil create -volname 'Super Cage' -srcfolder "$stage" -ov -format UDZO "$release/$artifact"
codesign --force --sign "$CAGE_SIGNING_IDENTITY" --timestamp "$release/$artifact"
codesign --verify --verbose=2 "$release/$artifact"
if [[ $notarize == 1 ]]; then
  notarize "$release/$artifact" "$release/dmg-notarization.json"
  xcrun stapler staple "$release/$artifact"
  xcrun stapler validate "$release/$artifact"
  spctl --assess --type open --context context:primary-signature --verbose=2 "$release/$artifact"
fi

# Distribute the corresponding unmodified Mihomo source beside its executable.
core_version="$(sed -n 's/^version=//p' "$source_root/scripts/fetch-proxy-core.sh")"
core_source="mihomo-$core_version-source.tar.gz"
curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
  "https://github.com/MetaCubeX/mihomo/archive/refs/tags/$core_version.tar.gz" \
  -o "$release/$core_source.part"
mv "$release/$core_source.part" "$release/$core_source"
tar -tzf "$release/$core_source" > /dev/null
(cd "$release" && shasum -a 256 "$artifact" "$core_source" "$guest_source" > SHA256SUMS)
# A caller that chose the App's location also owns the other copies.
if [[ -z "${CAGE_BUILD_OUTPUT:-}" ]]; then
  node "$source_root/scripts/cleanup-apps.mjs" --keep "$bundle" --root "$source_root/_output"
fi
if [[ $notarize == 1 ]]; then
  printf 'Verified release artifacts:\n'
else
  printf 'Signed but NOT notarized (testing only, never publish):\n'
fi
printf '%s/%s\n%s/%s\n%s/%s\n%s/SHA256SUMS\n' "$release" "$artifact" "$release" "$core_source" "$release" "$guest_source" "$release"
