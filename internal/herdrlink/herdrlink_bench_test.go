package herdrlink

import (
	"bufio"
	"io"
	"testing"
)

// BenchmarkKeystroke is one key from the owner through the link to the
// guest's core and its echo back, over in-memory pipes: the time the filter
// adds per keystroke, both ways (design 5.13). BenchmarkKeystrokePlain is the
// same over plain copies.
func BenchmarkKeystroke(b *testing.B)      { benchmarkEcho(b, true) }
func BenchmarkKeystrokePlain(b *testing.B) { benchmarkEcho(b, false) }

func benchmarkEcho(b *testing.B, filtered bool) {
	ownerR, ownerW := io.Pipe()
	toOwnerR, toOwnerW := io.Pipe()
	guestR, guestW := io.Pipe()
	toGuestR, toGuestW := io.Pipe()
	defer ownerW.Close()
	defer guestW.Close()
	if filtered {
		go func() { _, _ = Run(Options{}, ownerR, toOwnerW, guestR, toGuestW) }()
	} else {
		go func() { _, _ = io.Copy(toGuestW, ownerR) }()
		go func() { _, _ = io.Copy(toOwnerW, guestR) }()
	}
	// The guest's core: a hello reply, a stream for the attach, and an echo.
	go func() {
		r := bufio.NewReader(toGuestR)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(guestW, guestHello)
		for {
			channel, _, payload, err := readFrame(r, MaxPayload)
			if err != nil {
				return
			}
			if channel == 0 {
				_, _ = guestW.Write(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"channel\":3}}")))
				continue
			}
			_, _ = guestW.Write(Frame(channel, KindData, payload))
		}
	}()
	owner := bufio.NewReader(toOwnerR)
	_, _ = io.WriteString(ownerW, ownerHello)
	if _, err := owner.ReadString('\n'); err != nil {
		b.Fatal(err)
	}
	_, _ = ownerW.Write(call(7, "terminal.attach", "{\"terminal\":\"t1\"}"))
	if _, _, _, err := readFrame(owner, MaxPayload); err != nil {
		b.Fatal(err)
	}
	key := Frame(3, KindData, []byte("a"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ownerW.Write(key)
		if _, _, _, err := readFrame(owner, MaxPayload); err != nil {
			b.Fatal(err)
		}
	}
}
