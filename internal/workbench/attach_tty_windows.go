//go:build windows

package workbench

import (
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// attachLocalTTY is the console cage attach runs in. Windows has no SIGWINCH,
// so the size is read again a few times a second while attached.
func attachLocalTTY(stdin, stdout *os.File, stderr io.Writer) (attachTTY, func()) {
	resized := make(chan struct{}, 1)
	closed := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range signals {
			once.Do(func() { close(closed) })
		}
	}()
	tty := attachTTY{in: stdin, out: stdout, errOut: stderr, resized: resized, closed: closed}
	size := func() (int, int, bool) {
		cols, rows, err := term.GetSize(int(stdout.Fd()))
		return cols, rows, err == nil
	}
	tty.size = size
	go func() {
		lastCols, lastRows, _ := size()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if cols, rows, ok := size(); ok && (cols != lastCols || rows != lastRows) {
					lastCols, lastRows = cols, rows
					select {
					case resized <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	in := int(stdin.Fd())
	if term.IsTerminal(in) {
		tty.makeRaw = func() (func(), error) {
			state, err := term.MakeRaw(in)
			if err != nil {
				return nil, err
			}
			// The console must interpret the escape sequences it is sent.
			out := windows.Handle(stdout.Fd())
			var mode uint32
			restoreOut := func() {}
			if windows.GetConsoleMode(out, &mode) == nil {
				_ = windows.SetConsoleMode(out, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
				restoreOut = func() { _ = windows.SetConsoleMode(out, mode) }
			}
			return func() { restoreOut(); _ = term.Restore(in, state) }, nil
		}
	}
	return tty, func() { close(done); signal.Stop(signals); close(signals) }
}
