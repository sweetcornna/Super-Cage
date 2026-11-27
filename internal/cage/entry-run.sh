#!/bin/sh
# Super Cage 本机终端: runs one channel of the local SSH entry in this
# environment. Super Cage's controller calls it over its own SSH hop, as the
# environment's user, the same way it opens every Cage terminal:
#   run exec <command>   a command without a terminal (ssh host cmd, scp, rsync)
#   run shell            a login shell without a terminal (ssh -T host)
#   run sftp             the SFTP server
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

case "${1:-}" in
  exec)
    [ $# -eq 2 ] || exit 2
    if [ -d "$HOME/.local/share/cage/auto-shims" ]; then
      export PATH="$HOME/.local/share/cage/auto-shims:$PATH"
    fi
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
