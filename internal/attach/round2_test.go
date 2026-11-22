package attach

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Nothing the attach writes itself lands inside a clipboard write the
// program has open: turning focus reports back on after a reset, and the
// bell for an image that did not upload, wait for the string to end.
func TestOwnWritesWaitForAnOpenClipboardWrite(t *testing.T) {
	var out bytes.Buffer
	a := &attacher{filter: termfilter.New(termfilter.Options{ClipboardWrite: true}), tty: TTY{Out: &out}}
	if err := a.writeOutput([]byte("\x1bc\x1b]52;c;QUFB")); err != nil {
		t.Fatal(err)
	}
	a.bell()
	if got := out.String(); strings.Contains(got, focusOn) || strings.Contains(got, "\a") || !strings.HasSuffix(got, "\x1b]52;c;QUFB") {
		t.Fatalf("written inside the clipboard write: %q", got)
	}
	if err := a.writeOutput([]byte("QUFB\x07x")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasSuffix(got, "QUFB\ax"+focusOn+"\a") {
		t.Fatalf("not written after the clipboard write: %q", got)
	}
	// With nothing open, the bell rings at once.
	out.Reset()
	a.bell()
	if out.String() != "\a" {
		t.Fatalf("%q", out.String())
	}
}

// A connection id reaches a request header: only the launcher's alphabet.
func TestParseArgsRefusesAConnectionWithControls(t *testing.T) {
	for _, bad := range []string{"a\r\nX-Injected: 1", "a b", "a\x00", strings.Repeat("a", 129), "ä"} {
		if _, err := ParseArgs([]string{"--connection", bad, "--terminal", "0123456789abcdef"}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	cfg, err := ParseArgs([]string{"--connection", "Gen_1-x", "--terminal", "0123456789abcdef"})
	if err != nil || cfg.Target.Connection != "Gen_1-x" {
		t.Fatal(cfg, err)
	}
}
