//go:build darwin

package workbench

import (
	"testing"
	"time"
)

func TestParseHIDIdle(t *testing.T) {
	out := []byte("+-o IOHIDSystem  <class IOHIDSystem>\n    {\n      \"HIDIdleTime\" = 1830000000000\n    }\n")
	if d, err := parseHIDIdle(out); err != nil || d != 1830*time.Second {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := parseHIDIdle([]byte("{}")); err == nil {
		t.Fatal("missing idle time accepted")
	}
}
