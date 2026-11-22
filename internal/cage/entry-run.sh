#!/bin/sh
# Super Cage 本机终端: runs one channel of the local SSH entry in this
# environment. Super Cage's controller calls it over its own SSH hop, as the
# environment's user, the same way it opens every Cage terminal:
#   run exec <command>   a command without a terminal (ssh host cmd, scp, rsync)
#   run shell            a login shell without a terminal (ssh -T host)
#   run sftp             the SFTP server
#   run herdr <session>  Herdr's machine link: the environment's Herdr core
#   run herdr-exec <cmd> a Herdr install or update, in Herdr's environment
#   run herdr-idle <session> <seconds>
#                        stop that core later if nothing uses it then
# It only gives the channel the environment's own time zone and language, as
# Cage terminals get them. Nothing here is a security boundary: the
# environment's user can become root, so every rule that must hold against the
# environment is kept by Super Cage outside it.
set -u
environment=/etc/claude-cage/environment
if [ -r "$environment" ]; then
  set -a
  . "$environment"
  set +a
fi
export CAGE_WORKBENCH=1
cd "$HOME" 2>/dev/null || cd /

# A command ends with the connection that started it: when Super Cage cuts the
# connection, the SSH session that runs this script goes away and the command
# gets SIGHUP, as a terminal's programs do.
with_connection() {
  if setpriv --pdeathsig HUP -- true 2>/dev/null; then
    exec setpriv --pdeathsig HUP -- "$@"
  fi
  exec "$@"
}

# Herdr (design 5.5). Its core runs with this environment's settings and a
# runtime folder of Super Cage's own: with any folder but /run/user/<uid>
# Herdr starts the core as a plain process, not a systemd --user service
# with linger, and logind does not remove this one with the last login.
# Cage terminals find the core there (profile.d/cage-herdr.sh).
herdr_runtime="$HOME/.cache/cage-herdr/run"
herdr_env() {
  mkdir -p "$herdr_runtime" && chmod 700 "$herdr_runtime" || exit 1
  export XDG_RUNTIME_DIR="$herdr_runtime"
  if [ -d /usr/local/lib/cage-claude-bin ]; then
    case ":$PATH:" in
      *:/usr/local/lib/cage-claude-bin:*) ;;
      *) PATH="/usr/local/lib/cage-claude-bin:$PATH" ;;
    esac
  fi
  export PATH
}
herdr_session() {
  case "$1" in
    # Spelled out: a range such as a-z follows the locale's collation.
    ''|[!0123456789abcdefghijklmnopqrstuvwxyz]*|*[!0123456789abcdefghijklmnopqrstuvwxyz._-]*) exit 2 ;;
  esac
  [ ${#1} -le 32 ] || exit 2
  if [ "$1" = default ]; then
    unset HC_SESSION
  else
    export HC_SESSION="$1"
  fi
}
# The core as Herdr's own connect line finds it.
herdr_core() {
  core=$(command -v herdr-core 2>/dev/null) || core="$HOME/.local/bin/herdr-core"
  [ -x "$core" ]
}

case "${1:-}" in
  herdr)
    [ $# -eq 2 ] || exit 2
    herdr_session "$2"
    herdr_env
    if ! herdr_core; then
      echo "herdr-core is not installed here" >&2
      exit 127
    fi
    # One mark per open line: the idle check leaves the core alone while a
    # line's process lives. exec keeps this shell's PID.
    mkdir -p "$herdr_runtime/cage-lines" || exit 1
    : >"$herdr_runtime/cage-lines/$2.$$"
    with_connection "$core" connect
    ;;
  herdr-exec)
    [ $# -eq 2 ] || exit 2
    herdr_env
    with_connection /bin/sh -c "$2"
    ;;
  herdr-idle)
    [ $# -eq 3 ] || exit 2
    herdr_session "$2"
    case "$3" in ''|*[!0-9]*) exit 2 ;; esac
    unit="cage-herdr-idle-$2"
    # A transient timer runs nothing while it waits.
    if command -v systemd-run >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
      sudo -n systemctl stop "$unit.timer" "$unit.service" 2>/dev/null
      exec sudo -n systemd-run --quiet --collect --unit="$unit" --on-active="$3" \
        --uid="$(id -u)" --gid="$(id -g)" --setenv=HOME="$HOME" "$0" herdr-idle-check "$2"
    fi
    # No systemd (a test stand-in): one waiting process per session.
    herdr_env
    waiter="$herdr_runtime/cage-idle-$2.pid"
    if [ -s "$waiter" ]; then kill "$(cat "$waiter")" 2>/dev/null; fi
    (trap '' HUP; sleep "$3"; exec "$0" herdr-idle-check "$2") </dev/null >/dev/null 2>&1 &
    echo $! >"$waiter"
    exit 0
    ;;
  herdr-idle-check)
    [ $# -eq 2 ] || exit 2
    herdr_session "$2"
    herdr_env
    rm -f "$herdr_runtime/cage-idle-$2.pid"
    for mark in "$herdr_runtime/cage-lines/$2".*; do
      [ -e "$mark" ] || continue
      if kill -0 "${mark##*.}" 2>/dev/null; then
        exit 0
      fi
      rm -f "$mark"
    done
    herdr_core || exit 0
    # Asking a core that does not run would start one: only a live socket.
    [ -S "$herdr_runtime/herdr-core/$2.sock" ] || exit 0
    status=$("$core" server status --json 2>/dev/null) || exit 0
    case "$status" in
      *'"live":0,'* | *'"live":0}'*) "$core" server stop >/dev/null 2>&1 ;;
    esac
    exit 0
    ;;
  exec)
    [ $# -eq 2 ] || exit 2
    with_connection /bin/sh -c "$2"
    ;;
  shell)
    [ $# -eq 1 ] || exit 2
    with_connection "${SHELL:-/bin/sh}" -l
    ;;
  sftp)
    [ $# -eq 1 ] || exit 2
    for server in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server /usr/lib/ssh/sftp-server /usr/libexec/sftp-server; do
      if [ -x "$server" ]; then
        exec "$server"
      fi
    done
    echo "sftp-server is not installed in this environment" >&2
    exit 127
    ;;
esac
exit 2
