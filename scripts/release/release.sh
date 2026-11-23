#!/usr/bin/env bash
# Super Cage release driver for an Apple Silicon Mac. Steps and checks: docs/releasing.md.
#
#   release.sh prepare <version> [--base REF]
#   release.sh build <version> [--sha SHA] [--gh-run ID | --skip-gh] [--no-notarize]
#                    [--android | --no-android] [--android-code N] [--local-server]
#                    [--skip-mac] [--keep-clones] [--fresh]
#   release.sh assemble <version>
#   release.sh draft <version> --notes FILE [--allow-incomplete] [--dry-run]
#   release.sh publish <version> [--dry-run]
#   release.sh install <version>
#
# Personal values (signing identity, notarization key, Android keystore) come
# from the environment or ~/.config/super-cage/release.env; see release.env.example.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
driver="$(cd "$here/../.." && pwd -P)"
workflow=release-build.yml
artifacts=(release-windows release-linux release-server)
android_cert_default=e15628d8854a6cdb79d2ed506b3a015bf0c255483ec26b16f3e2c05fa3b77eb5
android_package=io.github.sweetcornna.cage.tenant.mobile
exec 3>&1

die() { printf 'release: %s\n' "$*" >&2; exit 1; }
say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*" >&3; }
tool() { node "$here/release-tool.mjs" "$@"; }
usage() { sed -n '2,14s/^# \{0,1\}//p' "${BASH_SOURCE[0]}" >&2; exit 2; }

config_keys=(CAGE_SIGNING_IDENTITY CAGE_NOTARY_PROFILE CAGE_NOTARY_KEYCHAIN CAGE_NOTARY_KEY CAGE_NOTARY_KEY_ID
  CAGE_NOTARY_ISSUER ANDROID_HOME JAVA_HOME CAGE_ANDROID_KEYSTORE CAGE_ANDROID_KEY_ALIAS CAGE_ANDROID_PASSWORD_FILE
  CAGE_ANDROID_CERT_SHA256 CAGE_BUILD_TOOLS CAGE_RELEASE_REPO CAGE_RELEASE_WORK CAGE_GH_TIMEOUT_MINUTES)
# The file fills in what the environment does not set.
load_config() {
  local file="${CAGE_RELEASE_CONFIG:-$HOME/.config/super-cage/release.env}" key saved=()
  if [[ ! -f "$file" ]]; then say "no $file: using the environment only"; return 0; fi
  [[ "$(stat -f %Lp "$file")" =~ ^[4-7]00$ ]] || die "$file must be readable by you only: chmod 600 '$file'"
  for key in "${config_keys[@]}"; do
    if [[ -n "${!key+x}" ]]; then saved+=("$key=${!key}"); fi
  done
  set -a
  # shellcheck source=/dev/null
  source "$file"
  set +a
  for key in ${saved[@]+"${saved[@]}"}; do export "${key%%=*}=${key#*=}"; done
}

check_version() {
  [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] || die "not a version: $1 (write 0.1.18, no v)"
}

# The main checkout owns .claude/worktrees; worktrees and clones share its objects.
setup_paths() {
  version="$1"
  check_version "$version"
  repo="$(dirname "$(git -C "$driver" rev-parse --path-format=absolute --git-common-dir)")"
  work="${CAGE_RELEASE_WORK:-$repo/.claude/worktrees}"
  rel="$work/release-$version.noindex"
  assets="$rel/assets"
  if [[ -n "${CAGE_RELEASE_REPO:-}" ]]; then
    gh_repo="$CAGE_RELEASE_REPO"
  else
    gh_repo="$(git -C "$repo" remote get-url origin | sed -E 's#^(https://github\.com/|git@github\.com:)##; s#\.git$##')"
  fi
  [[ "$gh_repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "cannot tell the GitHub repository; set CAGE_RELEASE_REPO=owner/name"
}

state_get() { if [[ -f "$rel/state" ]]; then sed -n "s/^$1=//p" "$rel/state" | tail -n 1; fi; }
state_set() {
  touch "$rel/state"
  { grep -v "^$1=" "$rel/state" || true; printf '%s=%s\n' "$1" "$2"; } > "$rel/state.new"
  mv "$rel/state.new" "$rel/state"
}

# step <name> <command...>: run one timed step; its output goes to the lane log.
# The command runs as a background job so that set -e still applies inside functions.
step() {
  local name="$1" start=$SECONDS status=0
  shift
  say "$name ..."
  "$@" &
  wait $! || status=$?
  printf '%s %s %s\n' "$name" "$status" "$((SECONDS - start))" >> "$rel/logs/steps"
  if [[ $status == 0 ]]; then say "$name ok ($((SECONDS - start)) s)"
  else say "$name FAILED (exit $status after $((SECONDS - start)) s)"; fi
  return "$status"
}

clone_at() { # <dir>: a clean shared clone of the release commit
  rm -rf "$1"
  git clone --quiet --shared --no-checkout "$repo" "$1"
  git -C "$1" checkout --quiet --detach "$sha"
  [[ "$(git -C "$1" rev-parse HEAD)" == "$sha" && -z "$(git -C "$1" status --porcelain)" ]]
}

# build-app.sh downloads the Mihomo core and checks it against the SHA-256 pinned
# in scripts/fetch-proxy-core.sh. Keep one verified copy between releases.
proxy_core() { # <checkout>: prints "version asset checksum" of the arm64 core
  local script="$1/scripts/fetch-proxy-core.sh" core checksum
  core="$(sed -n 's/^version=//p' "$script")"
  checksum="$(grep -E '^[[:space:]]*arm64\)' "$script" | grep -oE '[0-9a-f]{64}' || true)"
  if [[ -n "$core" && -n "$checksum" ]]; then printf '%s %s %s\n' "$core" "mihomo-darwin-arm64-$core.gz" "$checksum"; fi
}
seed_proxy_core() { # <checkout>
  local core asset checksum cache="$work/release-cache.noindex/proxy-core"
  read -r core asset checksum <<< "$(proxy_core "$1")" || return 0
  [[ -n "${checksum:-}" && -f "$cache/$asset" ]] || return 0
  if [[ "$(shasum -a 256 "$cache/$asset" | cut -d ' ' -f 1)" != "$checksum" ]]; then
    rm -f "$cache/$asset"; echo "dropped a cached $asset that does not match its pin"; return 0
  fi
  mkdir -p "$1/_output/proxy-core"
  cp "$cache/$asset" "$1/_output/proxy-core/$asset"
  if [[ -f "$cache/LICENSE-$core" ]]; then cp "$cache/LICENSE-$core" "$1/_output/proxy-core/LICENSE"; fi
  echo "Mihomo core from the cache: $asset"
}
save_proxy_core() { # <checkout>, after build-app.sh verified its download
  local core asset checksum cache="$work/release-cache.noindex/proxy-core"
  read -r core asset checksum <<< "$(proxy_core "$1")" || return 0
  [[ -n "${checksum:-}" && -f "$1/_output/proxy-core/$asset" ]] || return 0
  [[ "$(shasum -a 256 "$1/_output/proxy-core/$asset" | cut -d ' ' -f 1)" == "$checksum" ]] || return 0
  mkdir -p "$cache"
  cp "$1/_output/proxy-core/$asset" "$cache/$asset"
  if [[ -f "$1/_output/proxy-core/LICENSE" ]]; then cp "$1/_output/proxy-core/LICENSE" "$cache/LICENSE-$core"; fi
}

mac_lane() {
  local src="$rel/src-mac" out="$rel/mac"
  rm -rf "$out"
  mkdir -p "$out"
  step mac-clone clone_at "$src"
  seed_proxy_core "$src"
  step mac-build env SDKROOT="$sdk" CAGE_BUILD_OUTPUT="$out/Super Cage.app" bash "$src/scripts/build-app.sh"
  save_proxy_core "$src"
  # The packaging and its checks come from this checkout; the App, VERSION and pins from the release commit.
  step mac-package env CAGE_SOURCE_ROOT="$src" CAGE_BUILD_OUTPUT="$out/Super Cage.app" CAGE_RELEASE_OUTPUT="$out/release" \
    bash "$driver/scripts/package-macos.sh" --from-built-app ${package_flags[@]+"${package_flags[@]}"}
  step mac-metadata node "$src/scripts/release-version.mjs" metadata macos "$out/release-metadata.base.json"
  printf '%s\n' "$sha" > "$out/.done"
}

check_apk() { # <apk> [version code]
  local build_tools="$ANDROID_HOME/build-tools/${CAGE_BUILD_TOOLS:-35.0.0}" badging certificates digest
  local expected="${CAGE_ANDROID_CERT_SHA256:-$android_cert_default}" code="${2:-[0-9]*}"
  badging="$("$build_tools/aapt2" dump badging "$1")"
  if ! grep -qE "^package: name='$android_package' versionCode='$code' versionName='$version'( |$)" <<< "$badging"; then
    echo "unexpected package: $(head -n 1 <<< "$badging")"; return 1
  fi
  grep -q "^native-code: 'arm64-v8a'$" <<< "$badging" || { echo 'the APK is not arm64-v8a only'; return 1; }
  certificates="$("$build_tools/apksigner" verify --print-certs "$1")"
  [[ "$(grep -c 'certificate SHA-256 digest:' <<< "$certificates")" == 1 ]] || { echo 'expected exactly one signer'; return 1; }
  digest="$(sed -n 's/^Signer #1 certificate SHA-256 digest: //p' <<< "$certificates")"
  [[ "$digest" == "$expected" ]] || { echo "signed by $digest, expected $expected"; return 1; }
  # Called where set -e may not apply: every failure returns explicitly.
  "$build_tools/zipalign" -c -P 16 4 "$1" || { echo 'the APK is not aligned'; return 1; }
  echo "$(head -n 1 <<< "$badging" | cut -d ' ' -f 2-4); signer $digest"
}

android_lane() {
  local src="$rel/src-android" out="$rel/android" built
  rm -rf "$out"
  mkdir -p "$out"
  step android-clone clone_at "$src"
  step android-ui npm ci --prefix "$src/ui" --no-audit --no-fund
  step android-build env CAGE_ANDROID_VERSION_CODE="$android_code" bash "$src/mobile/build.sh" android-release
  for built in "$src/mobile/.build/android/Super-Cage-$version-android-arm64.apk" "$src/mobile/.build/android/Cage-$version-android-arm64.apk"; do
    if [[ -f "$built" ]]; then cp "$built" "$out/Super-Cage-$version-android-arm64.apk"; break; fi
  done
  step android-check check_apk "$out/Super-Cage-$version-android-arm64.apk" "$android_code"
  printf '%s\n' "$sha" > "$out/.done"
}

# Fallback for the server tarballs when GitHub Actions is unavailable (Go cross-builds, ~2 min).
server_lane() {
  local src="$rel/src-server" out="$rel/server"
  rm -rf "$out"
  mkdir -p "$out"
  step server-clone clone_at "$src"
  # shellcheck disable=SC2016 # $1 belongs to the inner shell
  step server-package bash -c 'cd "$1" && bash deploy/tenant/package-server.sh && bash deploy/tenant/package-novirt.sh' _ "$src"
  cp "$src"/_output/tenant-server.*/Super-Cage-Server-"$version"-linux-*.tar.gz "$src"/_output/tenant-novirt.*/Super-Cage-NoVirt-"$version"-linux-*.tar.gz "$out/"
  printf '%s\n' "$sha" > "$out/.done"
}

gh_dispatch() {
  local before output tries=0
  before="$(gh run list -R "$gh_repo" --workflow "$workflow" --limit 50 --json databaseId -q '.[].databaseId' 2> /dev/null || true)"
  output="$(gh workflow run "$workflow" -R "$gh_repo" -f sha="$sha" 2>&1)" ||
    die "could not start $workflow: $output (rerun with --gh-run ID, or --skip-gh to assemble without it)"
  gh_run="$(grep -oE 'actions/runs/[0-9]+' <<< "$output" | head -n 1 | cut -d / -f 3 || true)"
  while [[ -z "$gh_run" && $tries -lt 30 ]]; do
    sleep 3
    tries=$((tries + 1))
    gh_run="$(gh run list -R "$gh_repo" --workflow "$workflow" --event workflow_dispatch --limit 10 --json databaseId -q '.[].databaseId' |
      grep -vxF -f <(printf '%s\n' "$before") | head -n 1 || true)"
  done
  [[ -n "$gh_run" ]] || die "started $workflow but found no new run; pass --gh-run ID"
}

gh_wait() {
  local deadline=$((SECONDS + ${CAGE_GH_TIMEOUT_MINUTES:-90} * 60)) now last=''
  say "GitHub run https://github.com/$gh_repo/actions/runs/$gh_run"
  while :; do
    now="$(gh run view "$gh_run" -R "$gh_repo" --json status,conclusion,jobs \
      -q '[.status, .conclusion, ([.jobs[] | "\(.name): \(if .conclusion == "" then .status else .conclusion end)"] | join(", "))] | join("|")' 2> /dev/null || echo 'unknown||')"
    if [[ "$now" != "$last" ]]; then say "gh: ${now//|/ | }"; last="$now"; fi
    if [[ "${now%%|*}" == completed ]]; then break; fi
    if ((SECONDS > deadline)); then say "gh: still running after the timeout; assembling without it"; return 1; fi
    sleep 30
  done
  now="${now#*|}"
  [[ "${now%%|*}" == success ]]
}

gh_download() {
  local artifact status=0
  rm -rf "$rel/gh"
  for artifact in "${artifacts[@]}"; do
    if gh run download "$gh_run" -R "$gh_repo" -n "$artifact" -D "$rel/gh/$artifact" >> "$rel/logs/gh.log" 2>&1; then
      say "gh: downloaded $artifact: $(find "$rel/gh/$artifact" -type f | wc -l | tr -d ' ') files"
    else
      say "gh: no $artifact artifact"
      status=1
    fi
  done
  return "$status"
}

# Copy the files a lane made that match a pattern (an APFS clone where possible: no extra space).
copy_in() { cp -c "$1" "$assets/" 2> /dev/null || cp "$1" "$assets/"; }
take() { # <dir> <pattern>
  local found
  [[ -d "$1" ]] || return 0
  while IFS= read -r found; do copy_in "$found"; done < <(find "$1" -type f -name "$2")
}
lane_done() { [[ -f "$rel/$1/.done" && "$(cat "$rel/$1/.done")" == "$sha" ]]; }

assemble_assets() {
  local copies=() flags=() directory found
  rm -rf "$assets"
  mkdir -p "$assets"
  if lane_done mac; then
    take "$rel/mac/release" "Super-Cage-$version-macos-arm64.dmg"
    take "$rel/mac/release" "cage-macos-guest-$version-source.tar.gz"
    flags+=(--macos-metadata "$rel/mac/release-metadata.base.json")
    if [[ "$CAGE_SIGNING_IDENTITY" == 'Developer ID Application:'* ]]; then flags+=(--signed); fi
    if [[ "$(state_get notarized)" == 1 ]]; then flags+=(--notarized); fi
  fi
  if lane_done android; then take "$rel/android" "Super-Cage-$version-android-arm64.apk"; fi
  if [[ "$(state_get android)" == 1 ]]; then flags+=(--android); fi
  if lane_done server; then
    say "server tarballs from the local lane"
    take "$rel/server" "Super-Cage-*-$version-linux-*.tar.gz"
  else
    take "$rel/gh/release-server" "Super-Cage-*-$version-linux-*.tar.gz"
  fi
  take "$rel/gh/release-linux" "Super-Cage-$version-linux-amd64.deb"
  take "$rel/gh/release-linux" linux-desktop-build-metadata.json
  take "$rel/gh/release-windows" "Super-Cage-$version-windows-amd64-setup-unsigned.exe"
  take "$rel/gh/release-windows" 'qemu-*-source.tar.gz'
  take "$rel/gh/release-windows" windows-desktop-build-metadata.json
  # One Mihomo source is uploaded; every build's copy must be byte-identical to it.
  for directory in "$rel/gh/release-linux" "$rel/gh/release-windows" "$rel/mac/release"; do
    [[ -d "$directory" ]] || continue
    while IFS= read -r found; do copies+=("$found"); done < <(find "$directory" -type f -name 'mihomo-v*-source.tar.gz')
  done
  if ((${#copies[@]})); then copy_in "${copies[0]}"; fi
  for found in ${copies[@]+"${copies[@]}"}; do flags+=(--mihomo "$found"); done
  tool assemble "$assets" --version "$version" --sha "$sha" --git "$repo" ${flags[@]+"${flags[@]}"}
}

# The fast checks on the assembled files (seconds each). Prints ok/FAIL/SKIPPED lines.
check_assets() {
  local failures=0 file name scratch team apple xattrs notarized_flag=()
  result() { if [[ $1 == 0 ]]; then printf 'ok      %s\n' "$2"; else printf 'FAIL    %s\n' "$2"; failures=$((failures + 1)); fi; }
  for file in SHA256SUMS SHA256SUMS-linux-desktop.txt SHA256SUMS-windows-desktop.txt; do
    [[ -f "$assets/$file" ]] || continue
    (cd "$assets" && shasum -a 256 -c "$file" > /dev/null)
    result $? "shasum -c $file"
  done
  scratch="$(mktemp -d "$rel/check.XXXXXX")"
  for file in "$assets"/Super-Cage-Server-"$version"-linux-*.tar.gz "$assets"/Super-Cage-NoVirt-"$version"-linux-*.tar.gz; do
    [[ -f "$file" ]] || continue
    name="$(basename "$file" .tar.gz)"
    # A tarball made on a Mac can carry AppleDouble ._ files and xattr pax headers; Linux sees both.
    if ! python3 -I -c '
import sys, tarfile
apple = xattrs = 0
with tarfile.open(sys.argv[1], "r:gz") as archive:
    for member in archive:
        apple += member.name.rstrip("/").split("/")[-1].startswith("._")
        xattrs += any(key.startswith(("LIBARCHIVE.xattr.", "SCHILY.xattr.")) for key in member.pax_headers)
print(apple, xattrs)' "$file" > "$scratch/macos-metadata"; then
      result 1 "$name.tar.gz reads"
      continue
    fi
    read -r apple xattrs < "$scratch/macos-metadata"
    [[ $apple == 0 ]]
    result $? "$name.tar.gz: no AppleDouble ._ entries ($apple)"
    [[ $xattrs == 0 ]]
    result $? "$name.tar.gz: no extended-attribute headers ($xattrs)"
    tar -xzf "$file" -C "$scratch" "$name/cage-tenant" &&
      go version -m "$scratch/$name/cage-tenant" | grep -qF "vcs.revision=$sha" &&
      go version -m "$scratch/$name/cage-tenant" | grep -qF 'vcs.modified=false'
    result $? "$name: cage-tenant built from $sha, unmodified"
  done
  rm -rf "$scratch"
  file="$assets/Super-Cage-$version-linux-amd64.deb"
  if [[ -f "$file" ]]; then
    name="$(ar t "$file" 2> /dev/null | tr '\n' ' ')"
    if [[ "$name" =~ ^debian-binary\ control\.tar\.[a-z]+\ data\.tar\.[a-z]+\ $ ]]; then name="0 $name"; else name="1 $name"; fi
    result "${name%% *}" "$(basename "$file") is a Debian package (${name#* })"
  fi
  file="$assets/Super-Cage-$version-macos-arm64.dmg"
  if [[ -f "$file" ]]; then
    if [[ "$(state_get notarized)" != 1 ]]; then notarized_flag=(--not-notarized); fi
    bash "$driver/scripts/verify-macos-release.sh" "$assets" ${notarized_flag[@]+"${notarized_flag[@]}"} > "$rel/logs/verify-macos.log" 2>&1
    result $? "verify-macos-release.sh: signature, version, corresponding source${notarized_flag[*]:+ (not notarized: no ticket, no Gatekeeper)}"
    team="$(sed -nE 's/.*\(([A-Z0-9]{10})\)$/\1/p' <<< "$CAGE_SIGNING_IDENTITY")"
    if [[ -n "$team" ]]; then
      bash "$here/check-dmg.sh" "$file" "$version" "$team" ${notarized_flag[@]+"${notarized_flag[@]}"} | sed 's/^/        dmg: /'
      result "${PIPESTATUS[0]}" "what released in-app updaters require of the DMG"
    else
      printf 'SKIPPED the updater checks: CAGE_SIGNING_IDENTITY names no team\n'
    fi
  fi
  file="$assets/Super-Cage-$version-android-arm64.apk"
  if [[ -f "$file" ]]; then
    if [[ -n "${ANDROID_HOME:-}" ]]; then
      check_apk "$file" "$(state_get android_code)" | sed 's/^/        apk: /'
      result "${PIPESTATUS[0]}" "APK package, version, ABI, signer and alignment"
    else
      printf 'SKIPPED the APK checks: ANDROID_HOME is not set\n'
    fi
  fi
  return $((failures > 0))
}

assemble_and_check() {
  local status=0 checks=0
  say 'assembling'
  assemble_assets 2>&1 | tee "$rel/logs/assemble.log" || status=${PIPESTATUS[0]}
  say 'checking'
  check_assets 2>&1 | tee "$rel/logs/checks.log" || checks=${PIPESTATUS[0]}
  if [[ $status == 1 || $checks != 0 ]]; then state_set checks failed; else state_set checks ok; fi
  if [[ $status == 0 ]]; then state_set complete 1; else state_set complete 0; fi
  [[ $status == 0 && $checks == 0 ]]
}

summary() {
  local name status seconds
  printf '\nStep timings (s):\n'
  while read -r name status seconds; do printf '  %-16s %5s  %s\n' "$name" "$seconds" "$([[ $status == 0 ]] && echo ok || echo "FAILED (exit $status)")"; done < "$rel/logs/steps"
  printf '\nAssets in %s:\n' "$assets"
  find "$assets" -type f -maxdepth 1 -exec stat -f '%z %N' {} + | sort -k 2 |
    awk '{ size = $1; sub(/^[0-9]+ .*\//, ""); printf "  %10.1f MiB  %s\n", size / 1048576, $0 }'
  printf '\nState: %s\n' "$(tr '\n' ' ' < "$rel/state")"
}

cmd_build() {
  local skip_gh=0 skip_mac=0 keep=0 fresh=0 local_server=0 android='' started=$SECONDS
  local mac_pid='' android_pid='' server_pid='' failed=0 needed lane
  sha='' gh_run='' android_code="$(date +%Y%m%d)01" package_flags=() notarize=1
  while (($#)); do
    case "$1" in
      --sha) sha="$2"; shift ;;
      --gh-run) gh_run="$2"; shift ;;
      --skip-gh) skip_gh=1 ;;
      --no-notarize) notarize=0; package_flags=(--no-notarize) ;;
      --android) android=1 ;;
      --no-android) android=0 ;;
      --android-code) android_code="$2"; shift ;;
      --local-server) local_server=1 ;;
      --skip-mac) skip_mac=1 ;;
      --keep-clones) keep=1 ;;
      --fresh) fresh=1 ;;
      *) usage ;;
    esac
    shift
  done
  [[ -z "$gh_run" || $skip_gh == 0 ]] || die '--gh-run and --skip-gh exclude each other'
  [[ -z "$gh_run" || "$gh_run" =~ ^[0-9]+$ ]] || die "not a run id: $gh_run"
  if [[ -z "$android" ]]; then if [[ -n "${CAGE_ANDROID_KEYSTORE:-}" ]]; then android=1; else android=0; fi; fi
  for needed in git node go xcrun hdiutil shasum uv; do command -v "$needed" > /dev/null || die "$needed is not installed"; done
  if [[ $skip_gh == 0 ]]; then command -v gh > /dev/null || die 'gh is not installed'; fi
  : "${CAGE_SIGNING_IDENTITY:?Set CAGE_SIGNING_IDENTITY (see scripts/release/release.env.example)}"
  if [[ $notarize == 1 && -z "${CAGE_NOTARY_PROFILE:-}" ]] &&
    [[ -z "${CAGE_NOTARY_KEY:-}" || -z "${CAGE_NOTARY_KEY_ID:-}" || -z "${CAGE_NOTARY_ISSUER:-}" ]]; then
    die 'set CAGE_NOTARY_KEY, CAGE_NOTARY_KEY_ID and CAGE_NOTARY_ISSUER (or CAGE_NOTARY_PROFILE), or pass --no-notarize for a test build'
  fi
  if [[ $android == 1 ]]; then
    : "${ANDROID_HOME:?Set ANDROID_HOME for the Android lane}" "${CAGE_ANDROID_KEYSTORE:?Set CAGE_ANDROID_KEYSTORE}"
    : "${CAGE_ANDROID_KEY_ALIAS:?Set CAGE_ANDROID_KEY_ALIAS}" "${CAGE_ANDROID_PASSWORD_FILE:?Set CAGE_ANDROID_PASSWORD_FILE}"
    [[ "$android_code" =~ ^[1-9][0-9]{0,9}$ ]] || die "not an Android version code: $android_code"
  fi

  say "fetching origin"
  git -C "$repo" fetch --quiet origin
  sha="${sha:-$(git -C "$repo" rev-parse origin/main)}"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || sha="$(git -C "$repo" rev-parse --verify "$sha^{commit}")"
  git -C "$repo" cat-file -e "$sha^{commit}" 2> /dev/null || die "unknown commit $sha"
  [[ "$(git -C "$repo" show "$sha:VERSION" | tr -d '\r\n')" == "$version" ]] || die "VERSION at $sha is not $version"
  if [[ -d "$rel" && "$(state_get sha)" != "$sha" ]]; then
    [[ $fresh == 1 ]] || die "$rel holds a build of $(state_get sha); pass --fresh to replace it"
    rm -rf "$rel"
  fi
  if [[ $skip_mac == 1 ]]; then lane_done mac || die "--skip-mac: $rel/mac holds no finished build of $sha"; fi
  mkdir -p "$rel/logs"
  : > "$rel/logs/steps"
  state_set version "$version"
  state_set sha "$sha"
  if [[ $skip_mac == 0 ]]; then state_set notarized "$notarize"; fi
  state_set android "$android"
  if [[ $android == 1 ]]; then state_set android_code "$android_code"; fi
  sdk="$(xcrun --sdk macosx --show-sdk-path)"
  export COPYFILE_DISABLE=1
  say "Super Cage $version at $sha into $rel"

  if [[ $skip_gh == 0 && -z "$gh_run" ]]; then gh_dispatch; fi
  if [[ -n "$gh_run" ]]; then state_set gh_run "$gh_run"; fi
  if [[ $skip_mac == 0 ]]; then (mac_lane) > "$rel/logs/mac.log" 2>&1 & mac_pid=$!; fi
  if [[ $android == 1 ]]; then (android_lane) > "$rel/logs/android.log" 2>&1 & android_pid=$!; fi
  if [[ $local_server == 1 ]]; then (server_lane) > "$rel/logs/server.log" 2>&1 & server_pid=$!; fi
  trap 'kill ${mac_pid} ${android_pid} ${server_pid} 2> /dev/null; exit 130' INT TERM

  if [[ -n "$gh_run" ]]; then
    local gh_start=$SECONDS gh_status=0
    gh_wait || gh_status=$?
    gh_download || gh_status=1
    printf '%s %s %s\n' github "$gh_status" "$((SECONDS - gh_start))" >> "$rel/logs/steps"
    if [[ $gh_status != 0 ]]; then failed=1; fi
  else
    say "no GitHub run: Windows, Linux and (without --local-server) server assets will be missing"
  fi
  for lane in mac android server; do
    local pid_var="${lane}_pid"
    [[ -n "${!pid_var}" ]] || continue
    if wait "${!pid_var}"; then say "$lane lane done"; else say "$lane lane FAILED: $rel/logs/$lane.log"; failed=1; fi
  done
  trap - INT TERM

  local status=0
  assemble_and_check || status=1
  if [[ $keep == 0 ]]; then rm -rf "$rel"/src-*; fi
  printf 'total %s\n' "$((SECONDS - started))" >> "$rel/logs/steps.total"
  summary
  printf 'Wall time: %d min %d s\n' "$(((SECONDS - started) / 60))" "$(((SECONDS - started) % 60))"
  if [[ $failed == 1 || $status == 1 ]]; then
    printf '\nNot ready: see the FAIL and MISSING lines above and %s/logs.\n' "$rel"
    return 1
  fi
  if [[ "$(state_get notarized)" != 1 ]]; then printf '\nTest build (not notarized): never draft or publish it.\n'; return 0; fi
  printf '\nNext: scripts/release/release.sh draft %s --notes <notes.md>\n' "$version"
}

cmd_assemble() {
  [[ $# == 0 ]] || usage
  sha="$(state_get sha)"
  [[ -n "$sha" ]] || die "no build in $rel"
  : > "$rel/logs/steps"
  assemble_and_check || { summary; return 1; }
  summary
}

ready_to_publish() { # <allow incomplete>
  sha="$(state_get sha)"
  [[ -n "$sha" && -d "$assets" ]] || die "no assembled build in $rel; run build first"
  [[ "$(state_get notarized)" == 1 ]] || die 'this build was made with --no-notarize; never publish it'
  [[ "$(state_get checks)" == ok ]] || die "the checks failed; see $rel/logs/checks.log"
  [[ "$(state_get complete)" == 1 || "$1" == 1 ]] || die "assets are missing (see $rel/logs/assemble.log); pass --allow-incomplete to release without them"
  local file
  for file in SHA256SUMS SHA256SUMS-linux-desktop.txt SHA256SUMS-windows-desktop.txt; do
    if [[ -f "$assets/$file" ]]; then (cd "$assets" && shasum -a 256 -c --quiet "$file") || die "$file does not match the assets"; fi
  done
}

release_json() { # <output>: the release as GitHub has it, drafts included
  gh release view "v$version" -R "$gh_repo" --json tagName,isDraft,targetCommitish,url,assets > "$1"
}

cmd_draft() {
  local notes='' dry=0 incomplete=0 files=() file json="$rel/release.json" names list
  while (($#)); do
    case "$1" in
      --notes) notes="$2"; shift ;;
      --dry-run) dry=1 ;;
      --allow-incomplete) incomplete=1 ;;
      *) usage ;;
    esac
    shift
  done
  [[ -f "$notes" ]] || die 'pass --notes FILE (the release notes, Markdown)'
  ready_to_publish "$incomplete"
  list="$(tool upload-list "$assets")" || die "$assets is not an assembled release"
  while IFS= read -r file; do files+=("$file"); done <<< "$list"
  if release_json "$json" 2> /dev/null; then
    [[ "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).isDraft)' "$json")" == true ]] ||
      die "v$version is already published"
    names="$(node -e 'for (const a of JSON.parse(require("fs").readFileSync(process.argv[1])).assets) console.log(a.name)' "$json")"
    local upload=()
    for file in "${files[@]}"; do grep -qxF "$(basename "$file")" <<< "$names" || upload+=("$file"); done
    say "draft v$version exists; uploading the ${#upload[@]} missing assets"
    if ((${#upload[@]})); then
      if [[ $dry == 1 ]]; then printf 'would run: gh release upload v%s -R %s %s\n' "$version" "$gh_repo" "${upload[*]}"; return 0; fi
      gh release upload "v$version" -R "$gh_repo" "${upload[@]}"
    fi
  else
    if [[ $dry == 1 ]]; then
      printf 'would run: gh release create v%s -R %s --draft --target %s --title "Super Cage %s" --notes-file %s <files>\n' "$version" "$gh_repo" "$sha" "$version" "$notes"
      printf 'with these %d files:\n' "${#files[@]}"
      printf '    %s\n' "${files[@]}"
      return 0
    fi
    gh release create "v$version" -R "$gh_repo" --draft --target "$sha" --title "Super Cage $version" --notes-file "$notes" "${files[@]}"
  fi
  release_json "$json"
  [[ "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).targetCommitish)' "$json")" == "$sha" ]] ||
    die "the draft does not target $sha"
  tool digests "$assets" "$json"
  printf 'Draft: %s\nReview it, then: scripts/release/release.sh publish %s\n' "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).url)' "$json")" "$version"
}

cmd_publish() {
  local dry=0 json="$rel/release.json" public
  while (($#)); do case "$1" in --dry-run) dry=1 ;; *) usage ;; esac; shift; done
  ready_to_publish 1
  release_json "$json" || die "no release v$version on GitHub; run draft first"
  field() { node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1]))[process.argv[2]])' "$json" "$1"; }
  [[ "$(field targetCommitish)" == "$sha" ]] || die "v$version targets $(field targetCommitish), not $sha"
  tool digests "$assets" "$json" || die 'the uploaded assets differ from the local ones'
  if [[ "$(field isDraft)" == true ]]; then
    if [[ $dry == 1 ]]; then printf 'would run: gh release edit v%s -R %s --draft=false --latest\n' "$version" "$gh_repo"; return 0; fi
    gh release edit "v$version" -R "$gh_repo" --draft=false --latest > /dev/null
  fi
  [[ "$(gh api "repos/$gh_repo/commits/v$version" -q .sha)" == "$sha" ]] || die "tag v$version does not point at $sha"
  [[ "$(gh api "repos/$gh_repo/releases/latest" -q .tag_name)" == "v$version" ]] || die "v$version is not the latest release"
  public="$(mktemp "$rel/public-sums.XXXXXX")"
  if ! curl -fsSL --retry 3 "https://github.com/$gh_repo/releases/download/v$version/SHA256SUMS" -o "$public" ||
    ! cmp -s "$public" "$assets/SHA256SUMS"; then
    rm -f "$public"
    die 'the public SHA256SUMS is missing or differs from the local one'
  fi
  rm -f "$public"
  printf 'Published v%s at %s; the public SHA256SUMS matches.\n' "$version" "$sha"
}

cmd_install() {
  [[ $# == 0 ]] || usage
  [[ "$(state_get notarized)" == 1 ]] || die 'this build was made with --no-notarize; install a notarized build'
  bash "$here/install-app.sh" "$assets/Super-Cage-$version-macos-arm64.dmg" "$work/app-backups.noindex"
}

cmd_prepare() {
  local base=origin/main current tree branch
  while (($#)); do case "$1" in --base) base="$2"; shift ;; *) usage ;; esac; shift; done
  git -C "$repo" fetch --quiet origin
  current="$(git -C "$repo" show "$base:VERSION" | tr -d '\r\n')"
  CAGE_RELEASE_VERSION_MODULE="$driver/scripts/release-version.mjs" node -e '
    import(require("url").pathToFileURL(process.env.CAGE_RELEASE_VERSION_MODULE).href)
      .then(m => process.exit(m.compareVersions(process.argv[1], process.argv[2]) > 0 ? 0 : 1))' \
    "$version" "$current" || die "$version is not newer than $current"
  if git -C "$repo" ls-remote --exit-code --tags origin "refs/tags/v$version" > /dev/null; then die "tag v$version exists"; fi
  tree="$work/release-$version-prepare" branch="release-$version"
  [[ ! -e "$tree" ]] || die "$tree exists"
  git -C "$repo" worktree add --quiet --no-track -b "$branch" "$tree" "$base"
  tool bump "$tree" "$version"
  (cd "$tree" && node scripts/check-version.mjs)
  git -C "$tree" add -u
  git -C "$tree" commit --quiet -m "Super Cage $version"
  cat <<EOF

Committed "Super Cage $version" on $branch in $tree ($current -> $version).
Read the "check" lines above: they mention $current outside release links; change them by hand if needed.
Next:
  git -C '$tree' push -u origin $branch
  gh pr create -R $gh_repo --base main --head $branch --title 'Super Cage $version' --body-file <notes.md>
  gh pr merge <number> -R $gh_repo --merge --match-head-commit \$(git -C '$tree' rev-parse HEAD)
  scripts/release/release.sh build $version
  git -C '$repo' worktree remove '$tree'
EOF
}

(($# >= 2)) || usage
command="$1" version="$2"
shift 2
load_config
setup_paths "$version"
case "$command" in
  prepare) cmd_prepare "$@" ;;
  build) cmd_build "$@" ;;
  assemble) cmd_assemble "$@" ;;
  draft) cmd_draft "$@" ;;
  publish) cmd_publish "$@" ;;
  install) cmd_install "$@" ;;
  *) usage ;;
esac
