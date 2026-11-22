#!/usr/bin/env bash
set -euo pipefail
# Runs as the guest user after ToolInstallPrelude, which defines the pins and
# cage_download_tool. Each refusal comes before the download, and all but the
# bubblewrap check before any change. The subshell lets an up-to-date install
# return early; configuration and provisioning follow it.
cage_install_claude_science() (
  set -euo pipefail
  machine="$(uname -m)"
  case "$machine" in
    x86_64|amd64) ;;
    aarch64|arm64)
      echo 'Claude Science 暂无 Linux arm64 官方版本，这个客机无法安装。只有 x86-64 Linux 客机可以使用 Claude Science。' >&2
      exit 1 ;;
    *)
      echo "Claude Science 只提供 x86-64 Linux 版本，这个客机的架构是 ${machine}，无法安装。" >&2
      exit 1 ;;
  esac
  if [ -e /lib/libc.musl-x86_64.so.1 ]; then
    echo 'Claude Science 的 Linux 版本需要 glibc，这个客机使用 musl，无法安装。' >&2
    exit 1
  fi
  mkdir -p "$HOME/.config/claude-cage"
  exec 9>"$HOME/.config/claude-cage/science-install.lock"
  flock -n 9 || { echo 'Claude Science 正在安装，请等待当前操作完成。' >&2; exit 1; }
  if systemctl is-active --quiet cage-claude-science.service; then
    echo '请先关闭 Claude Science，再更新或修复安装。' >&2
    exit 1
  fi
  target="$HOME/.local/bin/claude-science"
  wanted="$CAGE_CLAUDE_SCIENCE_VERSION"
  checksum="$CAGE_CLAUDE_SCIENCE_AMD64_SHA256"
  current=''
  if [ -x "$target" ]; then
    current="$(DO_NOT_TRACK=1 "$target" --version </dev/null 2>/dev/null | tr -s ' \t' '\n' | cage_tool_version)" || current=''
  fi
  if [ -n "$current" ]; then
    # A build older than the data directory can damage its sessions: never go back.
    if dpkg --compare-versions "$current" gt "$wanted"; then
      printf 'Claude Science %s 比 Super Cage 固定的 %s 更新，保留现有版本。\n' "$current" "$wanted"
      exit 0
    fi
    if [ "$current" = "$wanted" ] && printf '%s  %s\n' "$checksum" "$target" | sha256sum --check --status; then
      printf 'Claude Science %s 已安装，无需重新下载。\n' "$wanted"
      exit 0
    fi
  fi
  # The daemon refuses a data directory on tmpfs, overlayfs or ramfs (lost
  # on restart) unless started with --allow-ephemeral-data-dir; Cage never
  # passes it. Filesystem magic numbers as stat -f prints them.
  case "$(stat -f -c %t "$HOME" 2>/dev/null)" in
    1021994 | 794c7630 | 858458f6)
      printf '%s\n' '这个客机的用户目录位于重启后会清空的临时文件系统（tmpfs、overlayfs 或 ramfs），Claude Science 拒绝把项目和会话保存在这里，因此无法安装。请使用带持久磁盘的环境。' >&2
      exit 1
      ;;
  esac
  # The first start sets up Python and R environments of about 10 GB in the
  # data directory; an update only needs room for the download and its copy.
  required_mb=11264
  if [ -d "$HOME/.claude-science/conda" ]; then required_mb=1024; fi
  free_mb="$(df -Pk "$HOME" | awk 'NR == 2 { print int($4 / 1024) }')"
  if ! [ "${free_mb:-0}" -ge "$required_mb" ] 2>/dev/null; then
    printf '客机磁盘空间不足：安装 Claude Science 需要至少 %s MB 可用空间（首次启动还会准备约 10 GB 的 Python 与 R 环境），当前只有 %s MB。请先删除客机中不再需要的文件后重试。\n' "$required_mb" "${free_mb:-0}" >&2
    exit 1
  fi
  # Known-old bubblewrap from the package lists: refuse before installing anything.
  if [ "$(dpkg-query -W -f='${Status}' bubblewrap 2>/dev/null)" != 'install ok installed' ]; then
    candidate="$(apt-cache policy bubblewrap 2>/dev/null | awk '$1 == "Candidate:" { print $2; exit }')" || candidate=''
    if [ -n "$candidate" ] && [ "$candidate" != '(none)' ] && dpkg --compare-versions "$candidate" lt 0.8.0; then
      echo 'Claude Science 的沙箱需要 bubblewrap 0.8 或更高版本，这个客机系统提供的版本过旧（Ubuntu 22.04 即是如此），因此没有安装 Claude Science。请改用 Debian 13 客机。' >&2
      exit 1
    fi
  fi
  missing=()
  for package in ca-certificates curl bubblewrap socat python3; do
    if [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null)" != 'install ok installed' ]; then missing+=("$package"); fi
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    printf '正在安装 Claude Science 所需的系统组件：%s…\n' "${missing[*]}"
    sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 update </dev/null
    sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 install -y --no-install-recommends "${missing[@]}" </dev/null
  fi
  # The vendor's own feature test: a bubblewrap without --disable-userns
  # (older than 0.8.0, as on Ubuntu 22.04) cannot start the sandbox.
  probe="$(/usr/bin/bwrap --unshare-user --disable-userns --dev-bind / / -- /bin/true 2>&1 >/dev/null)" || true
  if printf '%s' "$probe" | grep -qiE 'unrecognized|unknown.*option'; then
    echo 'Claude Science 的沙箱需要 bubblewrap 0.8 或更高版本，这个客机系统提供的版本过旧（Ubuntu 22.04 即是如此），因此没有下载 Claude Science。请改用 Debian 13 客机。' >&2
    exit 1
  fi
  cache="$HOME/.cache/cage/downloads/$checksum"
  mkdir -p "$cache" "$HOME/.local/bin"
  exec 8>"$cache/lock"
  flock -w 60 8
  printf '正在下载 Claude Science %s（约 170 MB）…\n' "$wanted"
  cage_download_tool "https://storage.googleapis.com/operon-dist-cf94a20e-f71c-413c-bd00-9e12b1fedf59/operon-releases/$CAGE_CLAUDE_SCIENCE_BUILD/operon-linux-x64" "$checksum" "$cache/operon-linux-x64"
  stage="$(mktemp "$HOME/.local/bin/.claude-science-new-XXXXXX")"
  trap 'rm -f "$stage"' EXIT
  cp "$cache/operon-linux-x64" "$stage"
  chmod 0755 "$stage"
  # The copy that is about to run is the one checked, not only the download.
  printf '%s  %s\n' "$checksum" "$stage" | sha256sum --check --status || { echo 'Claude Science 安装文件校验失败，已停止安装。' >&2; exit 1; }
  installed="$(DO_NOT_TRACK=1 "$stage" --version </dev/null 2>/dev/null | tr -s ' \t' '\n' | cage_tool_version)" || installed=''
  if [ -z "$installed" ]; then
    echo 'Claude Science 安装文件无法在这个客机运行，已停止安装，原有文件保持不变。' >&2
    exit 1
  fi
  if [ "$installed" != "$wanted" ]; then
    printf 'Claude Science 安装文件报告的版本（%s）与固定版本 %s 不一致，已停止安装。\n' "$installed" "$wanted" >&2
    exit 1
  fi
  mv -f "$stage" "$target"
  trap - EXIT
  rm -rf "$cache"
  printf 'Claude Science %s 已安装。\n' "$wanted"
)
# Documented device settings: no telemetry and no background update. Super
# Cage, not the vendor updater, decides the version. Other keys are kept.
cage_configure_claude_science() {
  mkdir -p "$HOME/.claude-science"
  python3 - "$HOME/.claude-science/config.toml" <<'CAGE_SCIENCE_CONFIG'
import os, re, sys, tempfile
from pathlib import Path

path = Path(sys.argv[1])
lines = path.read_text(encoding='utf-8').splitlines() if path.exists() else []
header = re.compile(r'^\s*\[')
def setting(name):
    return re.compile(r'^\s*' + name + r'\s*=')

first = next((i for i, line in enumerate(lines) if header.match(line)), len(lines))
top, rest = lines[:first], lines[first:]
found = [i for i, line in enumerate(top) if setting('disable_telemetry').match(line)]
for i in found:
    top[i] = 'disable_telemetry = true'
if not found:
    while top and not top[-1].strip():
        top.pop()
    top.append('disable_telemetry = true')
    if rest:
        top.append('')

update = next((i for i, line in enumerate(rest) if re.match(r'^\s*\[\s*update\s*\]\s*(#.*)?$', line)), None)
if update is None:
    while rest and not rest[-1].strip():
        rest.pop()
    rest += ([''] if rest or top else []) + ['[update]', 'auto_update = false']
else:
    end = next((i for i in range(update + 1, len(rest)) if header.match(rest[i])), len(rest))
    found = [i for i in range(update + 1, end) if setting('auto_update').match(rest[i])]
    for i in found:
        rest[i] = 'auto_update = false'
    if not found:
        rest.insert(update + 1, 'auto_update = false')

text = '\n'.join(top + rest) + '\n'
mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
fd, name = tempfile.mkstemp(prefix='.config-', dir=path.parent)
with os.fdopen(fd, 'w', encoding='utf-8') as handle:
    handle.write(text)
os.chmod(name, mode)
os.replace(name, path)
CAGE_SCIENCE_CONFIG
}
cage_install_claude_science
cage_configure_claude_science
