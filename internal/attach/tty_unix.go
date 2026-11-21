//go:build !windows

package attach

import (
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// localTTY is the terminal cage attach runs in. Closing its window
// sends SIGHUP, which detaches; the Cage terminal keeps running.
func localTTY(stdin, stdout *os.File, stderr io.Writer) (TTY, func()) {
	resized := make(chan struct{}, 1)
	closed := make(chan struct{})
	var once sync.Once
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range signals {
			if s == syscall.SIGWINCH {
				select {
				case resized <- struct{}{}:
				default:
				}
				continue
			}
			once.Do(func() { close(closed) })
		}
	}()
	tty := TTY{In: stdin, Out: stdout, ErrOut: stderr, Resized: resized, Closed: closed}
	in := int(stdin.Fd())
	if term.IsTerminal(in) {
		tty.MakeRaw = func() (func(), error) {
			state, err := term.MakeRaw(in)
			if err != nil {
				return nil, err
			}
			return func() { _ = term.Restore(in, state) }, nil
		}
	}
	tty.Size = func() (int, int, bool) {
		for _, f := range []*os.File{stdout, stdin} {
			if cols, rows, err := term.GetSize(int(f.Fd())); err == nil {
				return cols, rows, true
			}
		}
		return 0, 0, false
	}
	return tty, func() { signal.Stop(signals); close(signals) }
}
