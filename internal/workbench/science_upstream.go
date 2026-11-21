package workbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Claude Science phase 2, upstream side (design §7.4 "Upstream"): one
// connection per HTTP connection to the guest daemon on guest loopback,
// carried by SSH, with the daemon credential held here only.

// scienceDialer opens a connection to the daemon's guest loopback port.
type scienceDialer interface {
	DialContext(ctx context.Context) (net.Conn, error)
	Mode() string
	Close() error
}

// sshEndpoint is the subset of an ssh_config Host block this dialer honours.
type sshEndpoint struct {
	Address    string
	User       string
	Identities []string
	KnownHosts string
	Strict     bool
	Alias      string
}

// parseSSHConfig reads the Host block for name. Anything it cannot evaluate
// faithfully (Match, Include, a missing field) is an error, and the caller
// then uses the ssh executable with the same file instead.
func parseSSHConfig(data []byte, name string) (sshEndpoint, error) {
	var endpoint sshEndpoint
	hostname, port := "", "22"
	inBlock, found := false, false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value := line, ""
		if i := strings.IndexAny(line, " \t="); i > 0 {
			key, value = line[:i], strings.TrimLeft(line[i:], " \t=")
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		switch strings.ToLower(key) {
		case "host":
			inBlock = false
			for _, pattern := range strings.Fields(value) {
				if pattern == name {
					inBlock, found = true, true
				}
			}
			continue
		case "match", "include":
			return endpoint, errors.New("ssh_config uses Match or Include")
		}
		if !inBlock {
			continue
		}
		switch strings.ToLower(key) {
		case "hostname":
			hostname = value
		case "port":
			port = value
		case "user":
			endpoint.User = value
		case "identityfile":
			endpoint.Identities = append(endpoint.Identities, value)
		case "userknownhostsfile":
			endpoint.KnownHosts = strings.Fields(value)[0]
		case "stricthostkeychecking":
			endpoint.Strict = strings.EqualFold(value, "yes")
		case "hostkeyalias":
			endpoint.Alias = value
		}
	}
	if !found || hostname == "" || endpoint.User == "" || len(endpoint.Identities) == 0 {
		return endpoint, errors.New("ssh_config lacks the host, user or identity")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return endpoint, errors.New("ssh_config port is invalid")
	}
	endpoint.Address = net.JoinHostPort(hostname, port)
	return endpoint, nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// scienceSSHDialer keeps one in-process SSH connection and opens a
// direct-tcpip channel per dial.
type scienceSSHDialer struct {
	endpoint sshEndpoint
	target   string
	mu       sync.Mutex
	client   *ssh.Client
	// pinned is the guest's host key, kept for this dialer's lifetime when
	// the config itself checks nothing (Lima: StrictHostKeyChecking no,
	// UserKnownHostsFile /dev/null). A changed key is then refused.
	pinned ssh.PublicKey
	closed bool
}

func newScienceSSHDialer(endpoint sshEndpoint, target string) *scienceSSHDialer {
	return &scienceSSHDialer{endpoint: endpoint, target: target}
}

func (d *scienceSSHDialer) Mode() string { return "in-process" }

func (d *scienceSSHDialer) clientConfig() (*ssh.ClientConfig, error) {
	var signers []ssh.Signer
	for _, path := range d.endpoint.Identities {
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		signers = append(signers, signer)
	}
	if len(signers) == 0 {
		return nil, errors.New("no usable SSH identity")
	}
	config := &ssh.ClientConfig{User: d.endpoint.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)}, Timeout: 5 * time.Second}
	if d.endpoint.Strict && d.endpoint.KnownHosts != "" && d.endpoint.KnownHosts != os.DevNull && d.endpoint.KnownHosts != "/dev/null" {
		check, err := knownhosts.New(expandHome(d.endpoint.KnownHosts))
		if err != nil {
			return nil, err
		}
		alias := d.endpoint.Alias
		config.HostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if alias != "" {
				hostname = alias
			}
			return check(hostname, remote, key)
		}
		return config, nil
	}
	config.HostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.pinned == nil {
			d.pinned = key
			return nil
		}
		if !bytes.Equal(d.pinned.Marshal(), key.Marshal()) {
			return errors.New("guest SSH host key changed")
		}
		return nil
	}
	return config, nil
}

func (d *scienceSSHDialer) connect(ctx context.Context) (*ssh.Client, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, net.ErrClosed
	}
	if d.client != nil {
		client := d.client
		d.mu.Unlock()
		return client, nil
	}
	d.mu.Unlock()
	config, err := d.clientConfig()
	if err != nil {
		return nil, err
	}
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", d.endpoint.Address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	conn, channels, requests, err := ssh.NewClientConn(raw, d.endpoint.Address, config)
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(conn, channels, requests)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		client.Close()
		return nil, net.ErrClosed
	}
	if d.client != nil {
		client.Close()
		return d.client, nil
	}
	d.client = client
	go func() {
		_ = client.Wait()
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
	}()
	return client, nil
}

func (d *scienceSSHDialer) DialContext(ctx context.Context) (net.Conn, error) {
	for attempt := 0; attempt < 2; attempt++ {
		client, err := d.connect(ctx)
		if err != nil {
			return nil, err
		}
		conn, err := client.DialContext(ctx, "tcp", d.target)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A dead connection: drop it and try once more on a new one.
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
		client.Close()
		var refused *ssh.OpenChannelError
		if errors.As(err, &refused) {
			return nil, err
		}
	}
	return nil, errors.New("guest SSH connection unavailable")
}

func (d *scienceSSHDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.client != nil {
		err := d.client.Close()
		d.client = nil
		return err
	}
	return nil
}

// scienceProcessDialer is the fallback: one "ssh -W" process per connection,
// as the browser control channel does (browser_ssh.go).
type scienceProcessDialer struct {
	s        *Server
	target   string
	lifetime context.Context
	cancel   context.CancelFunc
}

func (d *scienceProcessDialer) Mode() string { return "ssh-process" }

func (d *scienceProcessDialer) DialContext(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	processContext, cancel := context.WithCancel(d.lifetime)
	cmd := d.s.sshCommand(processContext, "-T", "-W", d.target, d.s.sshTarget())
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return nil, err
	}
	local, remote := net.Pipe()
	conn := &browserSSHConnection{Conn: local, cancel: cancel}
	if err := cmd.Start(); err != nil {
		conn.Close()
		remote.Close()
		return nil, err
	}
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = io.Copy(input, remote)
		_ = input.Close()
	}()
	go func() {
		_, _ = io.Copy(remote, output)
		_ = remote.Close()
		cancel()
		<-written
		_ = cmd.Wait()
	}()
	return conn, nil
}

func (d *scienceProcessDialer) Close() error { d.cancel(); return nil }

// scienceAutoDialer prefers the in-process client and falls back to the ssh
// executable for good once the in-process client cannot be set up.
type scienceAutoDialer struct {
	mu       sync.Mutex
	primary  scienceDialer
	fallback scienceDialer
	failed   bool
}

func (d *scienceAutoDialer) Mode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed || d.primary == nil {
		return d.fallback.Mode()
	}
	return d.primary.Mode()
}

func (d *scienceAutoDialer) DialContext(ctx context.Context) (net.Conn, error) {
	d.mu.Lock()
	primary := d.primary
	if d.failed {
		primary = nil
	}
	d.mu.Unlock()
	if primary != nil {
		conn, err := primary.DialContext(ctx)
		if err == nil {
			return conn, nil
		}
		var refused *ssh.OpenChannelError
		if ctx.Err() != nil || errors.As(err, &refused) {
			return nil, err
		}
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
		_ = primary.Close()
	}
	return d.fallback.DialContext(ctx)
}

func (d *scienceAutoDialer) Close() error {
	var err error
	if d.primary != nil {
		err = d.primary.Close()
	}
	return errors.Join(err, d.fallback.Close())
}

// newScienceDialer builds the dialer for this workload's Linux guest.
func (s *Server) newScienceDialer(port int) scienceDialer {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	life, cancel := context.WithCancel(s.lifetimeContext())
	auto := &scienceAutoDialer{fallback: &scienceProcessDialer{s: s, target: target, lifetime: life, cancel: cancel}}
	if data, err := os.ReadFile(filepath.Join(s.instanceDir(), "ssh.config")); err == nil {
		if endpoint, err := parseSSHConfig(data, s.sshTarget()); err == nil {
			auto.primary = newScienceSSHDialer(endpoint, target)
		}
	}
	return auto
}

// scienceUpstream holds the transport and the daemon credential.
type scienceUpstream struct {
	dialer    scienceDialer
	transport *http.Transport
	port      int
	// link returns a fresh single-use sign-in link of the running daemon.
	link func(context.Context) (string, error)

	mu     sync.Mutex
	bearer string
	mint   sync.Mutex
}

func newScienceUpstream(dialer scienceDialer, port int, link func(context.Context) (string, error)) *scienceUpstream {
	u := &scienceUpstream{dialer: dialer, port: port, link: link}
	u.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != u.hostPort() {
				return nil, errors.New("science upstream address refused")
			}
			return dialer.DialContext(ctx)
		},
		MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 90 * time.Second,
		DisableCompression: true, ForceAttemptHTTP2: false, MaxResponseHeaderBytes: 64 << 10,
	}
	return u
}

func (u *scienceUpstream) hostPort() string { return "localhost:" + strconv.Itoa(u.port) }
func (u *scienceUpstream) origin() string   { return "http://" + u.hostPort() }

// token returns the bearer, minting one when none is held.
func (u *scienceUpstream) token(ctx context.Context) (string, error) {
	u.mu.Lock()
	token := u.bearer
	u.mu.Unlock()
	if token != "" {
		return token, nil
	}
	return u.remint(ctx, "")
}

// remint replaces stale (or an absent bearer) once; concurrent callers share
// one exchange.
func (u *scienceUpstream) remint(ctx context.Context, stale string) (string, error) {
	u.mint.Lock()
	defer u.mint.Unlock()
	u.mu.Lock()
	if u.bearer != "" && u.bearer != stale {
		token := u.bearer
		u.mu.Unlock()
		return token, nil
	}
	u.bearer = ""
	u.mu.Unlock()
	token, err := u.exchange(ctx)
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	u.bearer = token
	u.mu.Unlock()
	return token, nil
}

var errScienceBearer = errors.New("Claude Science 没有接受访问凭证，请重新打开")

// exchange redeems a single-use link the way the daemon's own sign-in
// interstitial does, and keeps only the operon_auth value. Neither the link
// nor the value is logged or returned to any caller outside this file.
func (u *scienceUpstream) exchange(ctx context.Context) (string, error) {
	link, err := u.link(ctx)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(link)
	nonce := ""
	if err == nil && parsed.Port() == strconv.Itoa(u.port) {
		nonce = parsed.Query().Get("nonce")
	}
	if nonce == "" {
		return "", errScienceBearer
	}
	form := url.Values{"nonce": {nonce}, "dest": {"/"}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.origin()+"/api/auth/nonce", strings.NewReader(form))
	if err != nil {
		return "", errScienceBearer
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", u.origin())
	request.Header.Set("Accept", "text/html")
	response, err := u.transport.RoundTrip(request)
	if err != nil {
		return "", errors.New("无法连接环境内的 Claude Science，请确认它仍在运行")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode >= 400 {
		return "", errScienceBearer
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "operon_auth" && cookie.Value != "" && len(cookie.Value) <= 4096 && !strings.ContainsAny(cookie.Value, " \t\r\n\x00") {
			return cookie.Value, nil
		}
	}
	return "", errScienceBearer
}

// drop forgets the bearer (stop, lock, environment stop, VM change).
func (u *scienceUpstream) drop() {
	u.mu.Lock()
	u.bearer = ""
	u.mu.Unlock()
}

func (u *scienceUpstream) close() {
	u.drop()
	u.transport.CloseIdleConnections()
	_ = u.dialer.Close()
}

// RoundTrip injects the bearer and re-mints it once on a 401. The request
// arrives already rewritten by the front, with any body buffered (and so
// replayable through GetBody).
func (u *scienceUpstream) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := u.token(request.Context())
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		outbound := request.Clone(request.Context())
		if attempt > 0 && request.GetBody != nil {
			if outbound.Body, err = request.GetBody(); err != nil {
				return nil, err
			}
		}
		outbound.Header.Set("Authorization", "Bearer "+token)
		response, err := u.transport.RoundTrip(outbound)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || attempt > 0 || (request.Body != nil && request.Body != http.NoBody && request.GetBody == nil) {
			return response, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if token, err = u.remint(request.Context(), token); err != nil {
			return nil, err
		}
	}
}

// scienceSocket audits what the browser writes into an upgraded connection.
type scienceSocket struct {
	io.ReadWriteCloser
	audit *wsAudit
}

func (s scienceSocket) Write(p []byte) (int, error) { return s.audit.Write(p) }

func (s scienceSocket) CloseWrite() error {
	if stream, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return stream.CloseWrite()
	}
	return s.Close()
}

// wsAudit parses client-to-server WebSocket frames (RFC 6455, always masked),
// holds each frame until it is complete, scans data frames and forwards the
// original bytes unchanged. A hit refuses the frame and the connection.
type wsAudit struct {
	dst     io.Writer
	audit   *scienceAudit
	onHit   func(string)
	pending []byte
	carry   []byte
	failed  error
}

const wsAuditFrameLimit = 16 << 20

var errScienceSocketRefused = errors.New("science socket refused by the identity audit")

func (w *wsAudit) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	w.pending = append(w.pending, p...)
	for {
		size, payload, opcode, ok, err := wsFrame(w.pending)
		if err != nil {
			w.failed = err
			return 0, err
		}
		if !ok {
			break
		}
		if opcode < 8 {
			scanned := append(append([]byte{}, w.carry...), payload...)
			if category := w.audit.scan(scanned); category != "" {
				w.failed = errScienceSocketRefused
				if w.onHit != nil {
					w.onHit(category)
				}
				return 0, w.failed
			}
			if keep := w.audit.window(); len(scanned) > keep {
				w.carry = append([]byte{}, scanned[len(scanned)-keep:]...)
			} else {
				w.carry = scanned
			}
		}
		if _, err := w.dst.Write(w.pending[:size]); err != nil {
			w.failed = err
			return 0, err
		}
		w.pending = w.pending[size:]
	}
	if len(w.pending) > wsAuditFrameLimit+14 {
		w.failed = errors.New("science socket frame too large")
		return 0, w.failed
	}
	return len(p), nil
}

// wsFrame parses one frame at the start of data. It returns the frame's
// total size and its unmasked payload once the whole frame is present.
func wsFrame(data []byte) (size int, payload []byte, opcode byte, ok bool, err error) {
	if len(data) < 2 {
		return 0, nil, 0, false, nil
	}
	opcode = data[0] & 0x0f
	masked := data[1]&0x80 != 0
	length := uint64(data[1] & 0x7f)
	offset := 2
	switch length {
	case 126:
		if len(data) < 4 {
			return 0, nil, 0, false, nil
		}
		length, offset = uint64(binary.BigEndian.Uint16(data[2:4])), 4
	case 127:
		if len(data) < 10 {
			return 0, nil, 0, false, nil
		}
		length, offset = binary.BigEndian.Uint64(data[2:10]), 10
	}
	if !masked {
		return 0, nil, 0, false, errors.New("unmasked client frame")
	}
	if length > wsAuditFrameLimit {
		return 0, nil, 0, false, errors.New("science socket frame too large")
	}
	if len(data) < offset+4 {
		return 0, nil, 0, false, nil
	}
	key := data[offset : offset+4]
	offset += 4
	if uint64(len(data)-offset) < length {
		return 0, nil, 0, false, nil
	}
	payload = make([]byte, length)
	for i := range payload {
		payload[i] = data[offset+i] ^ key[i%4]
	}
	return offset + int(length), payload, opcode, true, nil
}

// readScienceBody buffers a request body for the audit, refusing encodings it
// cannot read and bodies over the limit.
func readScienceBody(r *http.Request, limit int64) ([]byte, int, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, 0, nil
	}
	if encoding := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
		return nil, http.StatusUnsupportedMediaType, errors.New("请求体编码不受支持")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if int64(len(data)) > limit {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("请求内容超过 %d MB，本机渲染暂不支持上传大文件", limit>>20)
	}
	return data, 0, nil
}
