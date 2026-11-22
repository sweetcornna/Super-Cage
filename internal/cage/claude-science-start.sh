#!/usr/bin/env bash
set -euo pipefail
# Starts the daemon unit and waits until it answers on its fixed port. The
# unit runs only on guest loopback; nothing here forwards or opens a port.
binary=/home/cage/.local/bin/claude-science
unit=cage-claude-science.service
port=47800
test -x "$binary" || { echo '请先安装 Claude Science。' >&2; exit 1; }
# Prints the daemon's port while it answers, nothing otherwise.
serving_port() {
  DO_NOT_TRACK=1 "$binary" status </dev/null 2>/dev/null | python3 -c 'import json, sys
try:
    state = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
if isinstance(state, dict) and state.get("running") is True and isinstance(state.get("port"), int):
    print(state["port"])' || true
}
if ! systemctl is-active --quiet "$unit"; then
  current="$(serving_port)"
  if [ -n "$current" ]; then
    printf '客机内已有一个不由 Super Cage 管理的 Claude Science 在端口 %s 运行。请在终端执行 claude-science stop 后重试。\n' "$current" >&2
    exit 1
  fi
  if [ -n "$(ss -Hltn "sport = :$port" 2>/dev/null)" ]; then
    printf '客机内另一个程序占用了 Claude Science 的端口 %s。请关闭该程序后重试。\n' "$port" >&2
    exit 1
  fi
  if systemctl is-failed --quiet "$unit"; then sudo systemctl reset-failed "$unit"; fi
  sudo systemctl start --no-block "$unit"
fi
for i in $(seq 1 240); do
  current="$(serving_port)"
  if [ "$current" = "$port" ]; then exit 0; fi
  if [ -n "$current" ]; then
    printf 'Claude Science 运行在端口 %s，不是 Super Cage 配置的 %s。请关闭 Claude Science 后重新打开。\n' "$current" "$port" >&2
    exit 1
  fi
  if systemctl is-failed --quiet "$unit"; then
    echo 'Claude Science 未能启动。它的沙箱需要 bubblewrap 0.8 或更高版本以及可用的用户命名空间；最近的日志如下，已隐去登录链接。' >&2
    sudo journalctl --namespace=cage -u "$unit" -n 30 --no-pager -o cat 2>/dev/null | sed -E 's/nonce=[^[:space:]&"]*/nonce=…/g; s/([?&](code|state)=)[^[:space:]&"#]*/\1…/g' >&2 || true
    exit 1
  fi
  sleep .5
done
echo 'Claude Science 启动超时。请稍后重试；仍失败时导出诊断包。' >&2
exit 1
