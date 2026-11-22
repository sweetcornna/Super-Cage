# Super Cage 本机终端: hc in this environment's terminals talks to the Herdr
# core that Herdr's app reaches through the local SSH entry (design 5.5).
# Only while that core runs: asked at a socket where none runs, hc starts a
# second core of its own.
if [ -z "${HC_SOCKET:-}" ] && [ -S "$HOME/.cache/cage-herdr/run/herdr-core/default.sock" ]; then
  export HC_SOCKET="$HOME/.cache/cage-herdr/run/herdr-core/default.sock"
fi
