package herdrlink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// The owner's side, modelled: how the owner's Herdr core (herdr-core 0.1.21)
// reads the bytes this filter writes to it. The other tests read the
// filter's output with the filter's own idea of where the hello reply ends
// and frames start; these read it the way the owner's core does, with a JSON
// reader that is not Go's. Where the two disagree, the environment chooses
// what the owner's core reads (review 3, H1).
//
//   - machines.rs dial_once: lines (read_line into a String: a line that is
//     not UTF-8 fails the dial) until one serde_json reads whose id is the
//     integer 1; every other line is skipped. A reply with an "error" member
//     fails the dial; any other reply switches the pipe to frames.
//   - remote.rs hold(): frames (wire.rs read_frame, 16 MiB at most). A
//     channel-0 DATA payload is read as one JSON value: one without a method
//     is a reply (the one whose id is the events.subscribe call's sets the
//     mirror channel to its result.channel, cut to u32), "$/cancelRequest"
//     cancels, and any other method is served by from_machine.
//   - remote.rs mirror_events: the mirror channel's bytes are JSON lines.

// ownerView is what the owner's core made of a link.
type ownerView struct {
	// helloEnd is where the owner's frames start; -1: it never got a reply.
	helloEnd int
	// starts are where the frames it read start.
	starts []int
	// calls are channel-0 calls from the machine the owner would serve.
	calls []string
	// events are the event lines its mirror applied, as read.
	events []any
	// mirrors are the channels it took for the mirror, in order.
	mirrors []uint32
	// end says why it stopped reading.
	end string
}

func readAsOwner(out []byte, subscribe uint64) ownerView {
	v := ownerView{helloEnd: -1}
	pos := 0
	for {
		if pos >= len(out) {
			v.end = "dial: no hello reply"
			return v
		}
		line := out[pos:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line = line[:i+1]
		}
		pos += len(line)
		if !utf8.Valid(line) {
			v.end = "dial: a line that is not UTF-8"
			return v
		}
		reply, err := serdeParse(line)
		if err != nil || !serdeIsUint(serdeField(reply, "id"), 1) {
			continue
		}
		if _, failed := serdeObject(reply)["error"]; failed {
			v.end = "dial: an error reply"
			return v
		}
		v.helloEnd = pos
		break
	}
	var mirror *uint32
	var pending []byte
	for {
		if len(out)-pos < HeaderSize {
			if pos == len(out) {
				v.end = "end of stream"
			} else {
				v.end = "a partial header"
			}
			return v
		}
		channel := binary.LittleEndian.Uint32(out[pos : pos+4])
		kind := out[pos+4]
		size := int(binary.LittleEndian.Uint32(out[pos+5 : pos+9]))
		if size > MaxPayload {
			v.end = "an oversized frame"
			return v
		}
		if len(out)-pos-HeaderSize < size {
			v.end = "a partial payload"
			return v
		}
		v.starts = append(v.starts, pos)
		payload := out[pos+HeaderSize : pos+HeaderSize+size]
		pos += HeaderSize + size
		switch {
		case channel == 0 && kind == KindData:
			message, err := serdeParse(payload)
			if err != nil {
				continue
			}
			method, isString := serdeField(message, "method").(string)
			switch {
			case !isString:
				if serdeIsUint(serdeField(message, "id"), subscribe) {
					mirror = nil
					if c, ok := serdeUint(serdeField(serdeField(message, "result"), "channel")); ok {
						cut := uint32(c)
						mirror = &cut
						v.mirrors = append(v.mirrors, cut)
					}
				}
			case method == "$/cancelRequest":
			default:
				v.calls = append(v.calls, method)
			}
		case channel != 0 && kind == KindData && mirror != nil && *mirror == channel:
			pending = append(pending, payload...)
			whole := bytes.LastIndexByte(pending, '\n') + 1
			for _, line := range bytes.Split(pending[:whole], []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				if event, err := serdeParse(line); err == nil {
					if method, _ := serdeField(event, "method").(string); method == "event" || method == "missed" {
						v.events = append(v.events, event)
					}
				}
			}
			pending = append([]byte(nil), pending[whole:]...)
		}
	}
}

// serdeNumber is a number as serde_json keeps it: an unsigned or a signed
// integer, or a float.
type serdeNumber struct {
	kind byte // 'u', 'i' or 'f'
	u    uint64
	i    int64
	f    float64
}

// serdeParse reads one JSON value as serde_json 1.0 (no arbitrary
// precision) does: UTF-8 only, no control characters in strings, no lone
// surrogates, at most 127 levels of nesting, numbers within f64's range,
// whitespace around the value only; a repeated key keeps the last value.
func serdeParse(data []byte) (any, error) {
	r := &serdeReader{data: data}
	value, err := r.value()
	if err != nil {
		return nil, err
	}
	r.space()
	if r.pos != len(r.data) {
		return nil, fmt.Errorf("trailing characters at %d", r.pos)
	}
	return value, nil
}

type serdeReader struct {
	data  []byte
	pos   int
	depth int
}

func (r *serdeReader) space() {
	for r.pos < len(r.data) {
		switch r.data[r.pos] {
		case ' ', '\n', '\t', '\r':
			r.pos++
		default:
			return
		}
	}
}

func (r *serdeReader) value() (any, error) {
	r.space()
	if r.pos >= len(r.data) {
		return nil, errors.New("EOF while parsing a value")
	}
	switch c := r.data[r.pos]; {
	case c == '{' || c == '[':
		r.depth++
		defer func() { r.depth-- }()
		if r.depth >= 128 {
			return nil, errors.New("recursion limit exceeded")
		}
		r.pos++
		if c == '{' {
			return r.object()
		}
		return r.array()
	case c == '"':
		r.pos++
		return r.str()
	case c == 't':
		return r.literal("true", true)
	case c == 'f':
		return r.literal("false", false)
	case c == 'n':
		return r.literal("null", nil)
	case c == '-' || c >= '0' && c <= '9':
		return r.number()
	}
	return nil, fmt.Errorf("expected value at %d", r.pos)
}

func (r *serdeReader) literal(word string, value any) (any, error) {
	if !bytes.HasPrefix(r.data[r.pos:], []byte(word)) {
		return nil, fmt.Errorf("expected %s at %d", word, r.pos)
	}
	r.pos += len(word)
	return value, nil
}

func (r *serdeReader) object() (any, error) {
	out := map[string]any{}
	r.space()
	if r.pos < len(r.data) && r.data[r.pos] == '}' {
		r.pos++
		return out, nil
	}
	for {
		r.space()
		if r.pos >= len(r.data) || r.data[r.pos] != '"' {
			return nil, fmt.Errorf("key must be a string at %d", r.pos)
		}
		r.pos++
		key, err := r.str()
		if err != nil {
			return nil, err
		}
		r.space()
		if r.pos >= len(r.data) || r.data[r.pos] != ':' {
			return nil, fmt.Errorf("expected : at %d", r.pos)
		}
		r.pos++
		value, err := r.value()
		if err != nil {
			return nil, err
		}
		out[key.(string)] = value
		r.space()
		if r.pos >= len(r.data) {
			return nil, errors.New("EOF while parsing an object")
		}
		switch r.data[r.pos] {
		case ',':
			r.pos++
		case '}':
			r.pos++
			return out, nil
		default:
			return nil, fmt.Errorf("expected , or } at %d", r.pos)
		}
	}
}

func (r *serdeReader) array() (any, error) {
	out := []any{}
	r.space()
	if r.pos < len(r.data) && r.data[r.pos] == ']' {
		r.pos++
		return out, nil
	}
	for {
		value, err := r.value()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
		r.space()
		if r.pos >= len(r.data) {
			return nil, errors.New("EOF while parsing a list")
		}
		switch r.data[r.pos] {
		case ',':
			r.pos++
		case ']':
			r.pos++
			return out, nil
		default:
			return nil, fmt.Errorf("expected , or ] at %d", r.pos)
		}
	}
}

func (r *serdeReader) str() (any, error) {
	var out []byte
	for {
		if r.pos >= len(r.data) {
			return nil, errors.New("EOF while parsing a string")
		}
		c := r.data[r.pos]
		switch {
		case c == '"':
			r.pos++
			return string(out), nil
		case c < 0x20:
			return nil, fmt.Errorf("control character in a string at %d", r.pos)
		case c == '\\':
			r.pos++
			if r.pos >= len(r.data) {
				return nil, errors.New("EOF in an escape")
			}
			e := r.data[r.pos]
			r.pos++
			switch e {
			case '"', '\\', '/':
				out = append(out, e)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				first, err := r.hex4()
				if err != nil {
					return nil, err
				}
				switch {
				case first >= 0xdc00 && first <= 0xdfff:
					return nil, errors.New("lone trailing surrogate in hex escape")
				case first >= 0xd800 && first <= 0xdbff:
					if !bytes.HasPrefix(r.data[r.pos:], []byte("\\u")) {
						return nil, errors.New("unexpected end of hex escape")
					}
					r.pos += 2
					second, err := r.hex4()
					if err != nil {
						return nil, err
					}
					if second < 0xdc00 || second > 0xdfff {
						return nil, errors.New("lone leading surrogate in hex escape")
					}
					out = utf8.AppendRune(out, rune(0x10000+(first-0xd800)<<10+(second-0xdc00)))
				default:
					out = utf8.AppendRune(out, rune(first))
				}
			default:
				return nil, fmt.Errorf("invalid escape at %d", r.pos)
			}
		case c < 0x80:
			out = append(out, c)
			r.pos++
		default:
			rn, size := utf8.DecodeRune(r.data[r.pos:])
			if rn == utf8.RuneError && size <= 1 {
				return nil, fmt.Errorf("invalid unicode code point at %d", r.pos)
			}
			out = append(out, r.data[r.pos:r.pos+size]...)
			r.pos += size
		}
	}
}

func (r *serdeReader) hex4() (int, error) {
	if len(r.data)-r.pos < 4 {
		return 0, errors.New("EOF in a hex escape")
	}
	n, err := strconv.ParseUint(string(r.data[r.pos:r.pos+4]), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid hex escape at %d", r.pos)
	}
	r.pos += 4
	return int(n), nil
}

func (r *serdeReader) number() (any, error) {
	start := r.pos
	digits := func() int {
		n := 0
		for r.pos < len(r.data) && r.data[r.pos] >= '0' && r.data[r.pos] <= '9' {
			r.pos++
			n++
		}
		return n
	}
	negative := r.data[r.pos] == '-'
	if negative {
		r.pos++
	}
	if r.pos >= len(r.data) {
		return nil, errors.New("EOF in a number")
	}
	if r.data[r.pos] == '0' {
		r.pos++
	} else if digits() == 0 {
		return nil, fmt.Errorf("invalid number at %d", start)
	}
	integer := true
	if r.pos < len(r.data) && r.data[r.pos] == '.' {
		integer = false
		r.pos++
		if digits() == 0 {
			return nil, fmt.Errorf("invalid number at %d", start)
		}
	}
	if r.pos < len(r.data) && (r.data[r.pos] == 'e' || r.data[r.pos] == 'E') {
		integer = false
		r.pos++
		if r.pos < len(r.data) && (r.data[r.pos] == '+' || r.data[r.pos] == '-') {
			r.pos++
		}
		if digits() == 0 {
			return nil, fmt.Errorf("invalid number at %d", start)
		}
	}
	text := string(r.data[start:r.pos])
	if integer {
		if !negative {
			if u, err := strconv.ParseUint(text, 10, 64); err == nil {
				return serdeNumber{kind: 'u', u: u}, nil
			}
		} else if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			if i == 0 {
				return serdeNumber{kind: 'u'}, nil
			}
			return serdeNumber{kind: 'i', i: i}, nil
		}
	}
	f, _ := strconv.ParseFloat(text, 64)
	if math.IsInf(f, 0) {
		return nil, fmt.Errorf("number out of range at %d", start)
	}
	return serdeNumber{kind: 'f', f: f}, nil
}

func serdeObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// serdeField is serde's v["key"]: null for a missing key or a non-object.
func serdeField(v any, key string) any { return serdeObject(v)[key] }

// serdeUint is serde's as_u64.
func serdeUint(v any) (uint64, bool) {
	n, ok := v.(serdeNumber)
	if !ok || n.kind != 'u' {
		return 0, false
	}
	return n.u, true
}

func serdeIsUint(v any, want uint64) bool {
	n, ok := serdeUint(v)
	return ok && n == want
}
