// Package controlwire carries the control transport of the slim cage-attach
// program: HTTP/1.1 requests and one WebSocket client over a loopback TCP
// connection to the controller. It exists because net/http and
// gorilla/websocket cost the program about 6 MB of resident memory before it
// does anything (net/http's initialisation alone is about 4 MB), half of its
// whole budget. It speaks only to the controller named by the private
// control record, which listens on 127.0.0.1, and implements just what that
// controller sends: no proxies, no redirects, no TLS, no compression.
package controlwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

const (
	// replyLimit bounds a JSON reply, as the desktop's client does.
	replyLimit = 256 << 10
	// headerLimit bounds the status line and headers of one reply.
	headerLimit = 64 << 10
)

// Client is an authenticated connection to one controller. Every request
// uses a fresh TCP connection and closes it, so nothing stays open or wakes
// up between requests.
type Client struct {
	record controlclient.ControlFile
	launch controlclient.LaunchResponse
	origin string // http://localhost:<port>, what the controller expects in Origin
	host   string // localhost:<port>
	addr   string // 127.0.0.1:<port>, where the controller listens

	// ConnectionID is sent with every request when set.
	ConnectionID string
	// Timeout bounds one request; zero means controlclient.RequestTimeout.
	Timeout time.Duration

	mu      sync.Mutex
	cookies []cookie
}

type cookie struct{ name, value string }

var _ controlclient.Conn = (*Client)(nil)

// Open reads a controller's private record without contacting it.
func Open(profile string) (*Client, error) {
	record, err := controlclient.ReadRecord(profile)
	if err != nil {
		return nil, err
	}
	return &Client{
		record: record,
		origin: controlclient.Origin(record),
		host:   fmt.Sprintf("localhost:%d", record.Port),
		addr:   fmt.Sprintf("127.0.0.1:%d", record.Port),
	}, nil
}

// Connect proves the launcher token to the controller of profile.
func Connect(profile string) (*Client, error) {
	c, launch, err := controlclient.Connect(profile, func(profile string) (controlclient.Conn, error) {
		c, err := Open(profile)
		if err != nil {
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	client := c.(*Client)
	client.launch = launch
	return client, nil
}

// ConnectConn is Connect for controlclient's agent sequence.
func ConnectConn(profile string) (controlclient.Conn, error) {
	c, err := Connect(profile)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Record() controlclient.ControlFile    { return c.record }
func (c *Client) Launch() controlclient.LaunchResponse { return c.launch }
func (c *Client) Origin() string                       { return c.origin }
func (c *Client) Close()                               {}

// SetTrustedViewCookie adds the desktop proof the local workbench uses.
// Protected requests still require a session and unlock.
func (c *Client) SetTrustedViewCookie(origin *url.URL) {
	c.setCookie(controlclient.TrustedViewCookie(origin.Port()), controlclient.TrustedViewProof(c.record.Token))
}

// CallContext sends a JSON request and decodes a 200 reply into output.
func (c *Client) CallContext(ctx context.Context, method, path string, input, output any) (int, error) {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return 0, err
		}
	}
	status, data, err := c.Do(ctx, method, path, body, true, c.Timeout)
	if err != nil {
		return 0, err
	}
	if status == 200 && output != nil {
		if err = json.Unmarshal(data, output); err != nil {
			return status, errors.New("后台响应无效，未继续更新。")
		}
	}
	return status, nil
}

// Do sends one request and returns the reply's status and at most 256 KiB of
// its body. A controller that cannot be reached, or a redirect, is a
// *url.Error, as net/http reports it.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, contentType bool, timeout time.Duration) (int, []byte, error) {
	if !validPath(path) {
		return 0, nil, errors.New("invalid request path")
	}
	if timeout <= 0 {
		timeout = controlclient.RequestTimeout
	}
	fail := func(err error) (int, []byte, error) {
		return 0, nil, &url.Error{Op: method, URL: c.origin + path, Err: err}
	}
	conn, err := c.dial(ctx, timeout)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	var request bytes.Buffer
	c.writeHead(&request, method, path)
	if contentType {
		request.WriteString("Content-Type: application/json\r\n")
	}
	if len(body) > 0 || method == "POST" || method == "PUT" || method == "PATCH" {
		request.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	request.WriteString("Connection: close\r\n\r\n")
	if _, err := conn.Write(request.Bytes()); err != nil {
		return fail(err)
	}
	if _, err := conn.Write(body); err != nil {
		return fail(err)
	}
	r := bufio.NewReader(conn)
	reply, err := readReply(r)
	if err != nil {
		return fail(err)
	}
	c.keepCookies(reply)
	if reply.status >= 300 && reply.status < 400 {
		return fail(errors.New("redirect refused"))
	}
	if method == "HEAD" || reply.status == 204 || reply.status == 304 || reply.status < 200 {
		return reply.status, nil, nil
	}
	data, err := readBody(r, reply, replyLimit)
	if err != nil {
		return fail(err)
	}
	return reply.status, data, nil
}

func (c *Client) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "tcp4", c.addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(deadline)
	return conn, nil
}

// writeHead writes the request line and the headers every request carries.
func (c *Client) writeHead(b *bytes.Buffer, method, path string) {
	b.WriteString(method + " " + path + " HTTP/1.1\r\nHost: " + c.host + "\r\nOrigin: " + c.origin + "\r\n")
	if c.ConnectionID != "" {
		b.WriteString(controlclient.ConnectionHeader + ": " + c.ConnectionID + "\r\n")
	}
	c.mu.Lock()
	if len(c.cookies) > 0 {
		b.WriteString("Cookie: ")
		for i, k := range c.cookies {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(k.name + "=" + k.value)
		}
		b.WriteString("\r\n")
	}
	c.mu.Unlock()
}

// validPath accepts an origin-relative request target without spaces or
// control characters, so it cannot add a header.
func validPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] <= ' ' || path[i] >= 0x7f {
			return false
		}
	}
	return true
}

func (c *Client) setCookie(name, value string) {
	if !validCookie(name, value) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.cookies {
		if c.cookies[i].name == name {
			c.cookies[i].value = value
			return
		}
	}
	c.cookies = append(c.cookies, cookie{name, value})
}

func (c *Client) deleteCookie(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.cookies {
		if c.cookies[i].name == name {
			c.cookies = append(c.cookies[:i], c.cookies[i+1:]...)
			return
		}
	}
}

// keepCookies applies the reply's Set-Cookie headers. The controller sets
// host-only cookies for path / and clears one with Max-Age or Expires.
func (c *Client) keepCookies(reply reply) {
	for _, line := range reply.cookies {
		parts := strings.Split(line, ";")
		name, value, ok := strings.Cut(strings.TrimSpace(parts[0]), "=")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.Trim(strings.TrimSpace(value), "\"")
		expired := false
		for _, attribute := range parts[1:] {
			key, v, _ := strings.Cut(strings.TrimSpace(attribute), "=")
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "max-age":
				if age, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && age <= 0 {
					expired = true
				}
			case "expires":
				if at, err := time.Parse(time.RFC1123, strings.TrimSpace(v)); err == nil && !at.After(time.Now()) {
					expired = true
				}
			}
		}
		if expired {
			c.deleteCookie(name)
		} else {
			c.setCookie(name, value)
		}
	}
}

func validCookie(name, value string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] <= ' ' || name[i] >= 0x7f || strings.IndexByte("()<>@,;:\\\"/[]?={}", name[i]) >= 0 {
			return false
		}
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] >= 0x7f || value[i] == ';' || value[i] == ',' || value[i] == '\\' || value[i] == '"' {
			return false
		}
	}
	return true
}

// reply is a parsed status line and the headers this client reads.
type reply struct {
	status        int
	contentLength int64 // -1 when absent
	chunked       bool
	cookies       []string
	header        map[string]string // other single-valued headers, lower-case names
}

func readReply(r *bufio.Reader) (reply, error) {
	out := reply{contentLength: -1, header: map[string]string{}}
	budget := headerLimit
	line, err := readLine(r, &budget)
	if err != nil {
		return out, err
	}
	version, rest, _ := strings.Cut(line, " ")
	code, _, _ := strings.Cut(rest, " ")
	if !strings.HasPrefix(version, "HTTP/1.") || len(code) != 3 {
		return out, errors.New("malformed reply")
	}
	if out.status, err = strconv.Atoi(code); err != nil || out.status < 100 {
		return out, errors.New("malformed reply status")
	}
	for {
		line, err := readLine(r, &budget)
		if err != nil {
			return out, err
		}
		if line == "" {
			return out, nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return out, errors.New("malformed reply header")
		}
		name, value = strings.ToLower(name), strings.TrimSpace(value)
		switch name {
		case "set-cookie":
			out.cookies = append(out.cookies, value)
		case "content-length":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || (out.contentLength >= 0 && out.contentLength != n) {
				return out, errors.New("malformed content length")
			}
			out.contentLength = n
		case "transfer-encoding":
			if !strings.EqualFold(value, "chunked") {
				return out, errors.New("unsupported transfer encoding")
			}
			out.chunked = true
		default:
			if previous, seen := out.header[name]; seen {
				value = previous + ", " + value
			}
			out.header[name] = value
		}
	}
}

// readLine reads one CRLF-terminated line within the remaining budget.
func readLine(r *bufio.Reader, budget *int) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		*budget -= len(chunk)
		if *budget < 0 {
			return "", errors.New("reply headers too large")
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		break
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// readBody reads at most limit bytes of the reply's body; the rest is left
// unread, as a limited reader would.
func readBody(r *bufio.Reader, reply reply, limit int) ([]byte, error) {
	var body io.Reader
	switch {
	case reply.chunked:
		body = &chunkedReader{r: r}
	case reply.contentLength >= 0:
		body = io.LimitReader(r, reply.contentLength)
	default:
		body = r // until the controller closes the connection
	}
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)))
	if err != nil {
		return nil, err
	}
	if !reply.chunked && reply.contentLength > int64(len(data)) && len(data) < limit {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

// chunkedReader decodes a chunked body.
type chunkedReader struct {
	r    *bufio.Reader
	left int64
	done bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.left == 0 {
		budget := headerLimit
		line, err := readLine(c.r, &budget)
		if err != nil {
			return 0, err
		}
		size, _, _ := strings.Cut(line, ";")
		n, err := strconv.ParseInt(strings.TrimSpace(size), 16, 64)
		if err != nil || n < 0 {
			return 0, errors.New("malformed chunk")
		}
		if n == 0 {
			c.done = true
			for { // trailers end with an empty line
				line, err := readLine(c.r, &budget)
				if err != nil || line == "" {
					return 0, io.EOF
				}
			}
		}
		c.left = n
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left == 0 && err == nil {
		budget := 2
		if line, lineErr := readLine(c.r, &budget); lineErr != nil || line != "" {
			return n, errors.New("malformed chunk end")
		}
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
