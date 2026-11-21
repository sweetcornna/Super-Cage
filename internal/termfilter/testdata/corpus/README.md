# Filter corpus

Output of real programs, recorded on macOS through a pseudo-terminal
(100x30, TERM=xterm-256color) by scratchpad corpus/record.py: start the
program, use it briefly, quit.

- vim.bin: vim 9 (`vim -u DEFAULTS`), insert text, :q!
- less.bin: less on a long file, scroll, search, q
- top.bin: top -l 0 style refresh, q
- tmux.bin: tmux on a private socket, split, type, exit

Stand-ins, built from the sequences the libraries write (no network
program was run):

- ink.bin: Claude Code (Ink): cursor hide, bracketed paste, focus,
  kitty keyboard push, background query, DA1, title, synchronized frames
  with log-update's erase-lines, SGR with truecolor and curly underline,
  OSC 8, OSC 9 and 777 notifications, OSC 9;4 progress, BEL, clear, exit.
- crossterm.bin: Codex TUI (ratatui on crossterm): keyboard enhancement
  probe (CSI ?u then DA1), cursor position, OSC 10/11 queries, bracketed
  paste, focus change, PushKeyboardEnhancementFlags(7), synchronized
  frames, cursor style, insert_history (DECSC, DECSTBM, RI, SU, SD,
  DECRC), alternate screen with EnableMouseCapture, exit.

corpus_test.go names every sequence the filter drops from them and why.
