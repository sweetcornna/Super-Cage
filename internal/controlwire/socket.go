package controlwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// Message types of a WebSocket (RFC 6455).
const (
	TextMessage   = 1
	BinaryMessage = 2

	opContinuation = 0
	opClose        = 8
	opPing         = 9
	opPong         = 10
)

const handshakeTimeout = 15 * time.Second

// controlWriteWait bounds the reply to a ping and the closing frame.
const controlWriteWait = 10 * time.Second

// CloseError is the close frame that ended a stream.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	return "websocket: close " + strings.TrimSpace(itoa(e.Code)+" "+e.Text)
}

// Conn is one client WebSocket. One goroutine reads; writes may come from
// any goroutine and are serialised, including the answers to pings.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader

	wmu       sync.Mutex
	wbuf      []byte
	closeSent bool

	// The data message being read.
	left    int64 // unread payload of the current frame
	final   bool  // the current frame ends its message
	reading bool  // a message is open
	err     error // a read error ends the stream for good
}

// Dial opens a workload WebSocket on the authenticated controller
// connection, exactly as the UI does, without exporting its cookies.
func (c *Client) Dial(ctx context.Context, target controlclient.AgentTarget, path string) (*Conn, error) {
	conn, _, err := c.DialHandshake(ctx, target, path)
	return conn, err
}

// handshakeBodyLimit bounds the body of a refused handshake that
// DialHandshake reads.
const handshakeBodyLimit = 4 << 10

// Handshake is the controller's answer to a WebSocket handshake: its status,
// its single-valued headers by lower-case name and, when it refused the
// stream, at most 4 KiB of the body that says why. Status is 0 when no
// answer was read.
type Handshake struct {
	Status int
	Header map[string]string
	Body   []byte
}

// DialHandshake is Dial for callers that read the answer itself: the SSH
// entry's proxy takes the host key from a header and its refusal from the
// body. A refusal returns the same error as Dial.
func (c *Client) DialHandshake(ctx context.Context, target controlclient.AgentTarget, path string) (*Conn, Handshake, error) {
	var answer Handshake
	if err := controlclient.CheckSocket(target, path); err != nil {
		return nil, answer, err
	}
	if !validPath(path) {
		return nil, answer, &controlclient.AgentError{Code: "invalid_input", Message: "Use a relative /api/ WebSocket path", NextAction: "routes"}
	}
	var request bytes.Buffer
	// The generation is the target's, as the UI sends it.
	if err := c.writeHead(&request, "GET", path, controlclient.Connection(target)); err != nil {
		return nil, answer, &controlclient.AgentError{Code: "invalid_input", Message: "Use a connection generation without control characters", NextAction: "client.context"}
	}
	failed := &controlclient.AgentError{Code: "connection_failed", Message: "WebSocket connection failed", NextAction: "status"}
	conn, err := c.dial(ctx, handshakeTimeout)
	if identity := controlclient.IdentityAgentError(err); identity != nil {
		return nil, answer, identity
	}
	if err != nil {
		return nil, answer, failed
	}
	ws, answer, err := upgrade(ctx, conn, &request)
	switch {
	case err == nil:
		return ws, answer, nil
	case answer.Status != 0 && answer.Status != 101:
		return nil, answer, controlclient.HTTPError(answer.Status)
	}
	return nil, answer, failed
}

// UpgradeConn opens a WebSocket on conn, a connection already made to host
// (the cloud entry's TLS connection to the portal's gateway), with the
// request line, Host and the given headers, and nothing else: no Origin, no
// cookie. A refusal returns the answer, its status and at most 4 KiB of its
// body, with an error.
func UpgradeConn(ctx context.Context, conn net.Conn, host, path string, headers [][2]string) (*Conn, Handshake, error) {
	if !validPath(path) || hasControl(host) || host == "" {
		conn.Close()
		return nil, Handshake{}, errors.New("invalid WebSocket request")
	}
	var request bytes.Buffer
	request.WriteString("GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\n")
	for _, header := range headers {
		if header[0] == "" || strings.ContainsAny(header[0], ": \t") || hasControl(header[0]) || hasControl(header[1]) {
			conn.Close()
			return nil, Handshake{}, errors.New("invalid WebSocket request header")
		}
		request.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	return upgrade(ctx, conn, &request)
}

var errHandshake = errors.New("websocket handshake failed")

// upgrade finishes a client handshake whose request head is in request and
// closes conn when it fails.
func upgrade(ctx context.Context, conn net.Conn, request *bytes.Buffer) (*Conn, Handshake, error) {
	var answer Handshake
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	request.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n\r\n")
	r := bufio.NewReaderSize(conn, 4096)
	var reply reply
	_, err := conn.Write(request.Bytes())
	if err == nil {
		reply, err = readReply(r)
	}
	if err == nil {
		answer.Status, answer.Header = reply.status, reply.header
		if reply.status != 101 && (reply.chunked || reply.contentLength >= 0) {
			// Bounded by the dial's deadline, which still holds.
			answer.Body, _ = readBody(r, reply, handshakeBodyLimit)
		}
	}
	if !stop() || err != nil {
		conn.Close()
		return nil, Handshake{}, errHandshake
	}
	if reply.status != 101 {
		conn.Close()
		return nil, answer, errHandshake
	}
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if !strings.EqualFold(reply.header["upgrade"], "websocket") || !hasToken(reply.header["connection"], "upgrade") ||
		reply.header["sec-websocket-accept"] != base64.StdEncoding.EncodeToString(digest[:]) ||
		reply.header["sec-websocket-extensions"] != "" || reply.header["sec-websocket-protocol"] != "" {
		conn.Close()
		return nil, Handshake{}, errHandshake
	}
	_ = conn.SetDeadline(time.Time{})
	// Frames that arrived with the handshake are already in r.
	return &Conn{conn: conn, r: r}, answer, nil
}

func hasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// NetConn is the underlying connection; tests drop it to simulate a network
// failure.
func (c *Conn) NetConn() net.Conn { return c.conn }

// SetReadDeadline bounds the reads that follow.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the connection without a closing handshake.
func (c *Conn) Close() error { return c.conn.Close() }

// NextReader returns the type of the next data message and a reader over
// its payload, which may span several frames. Pings are answered on the way.
// The previous message's unread payload is discarded.
func (c *Conn) NextReader() (int, io.Reader, error) {
	for c.err == nil && c.reading {
		if _, err := io.Copy(io.Discard, messageReader{c}); err != nil {
			break
		}
	}
	if c.err != nil {
		return 0, nil, c.err
	}
	for {
		op, err := c.nextFrame()
		if err != nil {
			return 0, nil, c.fail(err)
		}
		switch op {
		case TextMessage, BinaryMessage:
			c.reading = true
			return op, messageReader{c}, nil
		default:
			return 0, nil, c.fail(errors.New("websocket: continuation without a message"))
		}
	}
}

// ReadMessage reads a whole data message of at most limit bytes.
func (c *Conn) ReadMessage(limit int64) (int, []byte, error) {
	op, r, err := c.NextReader()
	if err != nil {
		return 0, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = c.fail(errors.New("websocket: message too large"))
	}
	return op, data, err
}

func (c *Conn) fail(err error) error {
	if c.err == nil {
		c.err = err
	}
	return c.err
}

// nextFrame reads frame headers until a data frame starts, answering control
// frames, and returns its opcode.
func (c *Conn) nextFrame() (int, error) {
	for {
		var head [2]byte
		if _, err := io.ReadFull(c.r, head[:]); err != nil {
			return 0, err
		}
		fin, op := head[0]&0x80 != 0, int(head[0]&0x0f)
		if head[0]&0x70 != 0 {
			return 0, errors.New("websocket: reserved bits set")
		}
		if head[1]&0x80 != 0 {
			return 0, errors.New("websocket: masked frame from the server")
		}
		size := int64(head[1] & 0x7f)
		switch size {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, err
			}
			size = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, err
			}
			if ext[0]&0x80 != 0 {
				return 0, errors.New("websocket: invalid frame length")
			}
			size = int64(binary.BigEndian.Uint64(ext[:]))
		}
		switch op {
		case opContinuation, TextMessage, BinaryMessage:
			c.left, c.final = size, fin
			return op, nil
		case opClose, opPing, opPong:
			if !fin || size > 125 {
				return 0, errors.New("websocket: invalid control frame")
			}
			var payload [125]byte
			if _, err := io.ReadFull(c.r, payload[:size]); err != nil {
				return 0, err
			}
			switch op {
			case opPing:
				if err := c.writeControl(opPong, payload[:size]); err != nil {
					return 0, err
				}
			case opClose:
				closed := &CloseError{Code: 1005}
				if size >= 2 {
					closed.Code = int(binary.BigEndian.Uint16(payload[:2]))
					closed.Text = string(payload[2:size])
				}
				var echo []byte
				if size >= 2 {
					echo = payload[:2]
				}
				_ = c.writeControl(opClose, echo)
				return 0, closed
			}
		default:
			return 0, errors.New("websocket: unknown opcode")
		}
	}
}

// messageReader reads the payload of the open message across continuation
// frames.
type messageReader struct{ c *Conn }

func (m messageReader) Read(p []byte) (int, error) {
	c := m.c
	if c.err != nil {
		return 0, c.err
	}
	for c.left == 0 {
		if c.final {
			c.reading = false
			return 0, io.EOF
		}
		op, err := c.nextFrame()
		if err != nil {
			return 0, c.fail(err)
		}
		if op != opContinuation {
			return 0, c.fail(errors.New("websocket: new message inside a message"))
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return n, c.fail(err)
	}
	return n, nil
}

// SetWriteDeadline bounds the writes that follow.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// WriteMessage sends one data message in one masked frame.
func (c *Conn) WriteMessage(op int, data []byte) error {
	if op != TextMessage && op != BinaryMessage {
		return errors.New("websocket: invalid message type")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return errors.New("websocket: closed")
	}
	return c.writeFrame(op, data)
}

// WriteClose starts the closing handshake with a normal closure.
func (c *Conn) WriteClose() error {
	return c.writeControl(opClose, []byte{0x03, 0xe8})
}

func (c *Conn) writeControl(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return nil
	}
	if op == opClose {
		c.closeSent = true
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(controlWriteWait))
	return c.writeFrame(op, payload)
}

// writeFrame masks payload into the reused write buffer; wmu is held.
func (c *Conn) writeFrame(op int, payload []byte) error {
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	b := c.wbuf[:0]
	b = append(b, 0x80|byte(op))
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = append(b, 0x80|126, byte(n>>8), byte(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	b = append(b, key[:]...)
	start := len(b)
	b = append(b, payload...)
	for i := range b[start:] {
		b[start+i] ^= key[i&3]
	}
	_, err := c.conn.Write(b)
	if cap(b) <= 64<<10 {
		c.wbuf = b[:0]
	} else {
		c.wbuf = nil // a large paste does not stay resident
	}
	return err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
