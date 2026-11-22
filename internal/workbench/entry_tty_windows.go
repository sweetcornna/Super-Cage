package workbench

import (
	"errors"
	"io"
	"os"
)

// entryOpenTTY opens the console, where cage entry on asks.
func entryOpenTTY() (io.ReadWriteCloser, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return entryConsole{in, out}, nil
}

// entryConsole reads the console's input and writes its output.
type entryConsole struct{ in, out *os.File }

func (c entryConsole) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c entryConsole) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c entryConsole) Close() error                { return errors.Join(c.in.Close(), c.out.Close()) }
