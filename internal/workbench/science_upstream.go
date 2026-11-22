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
	// F10, round-2 N5: exchanges made because the daemon refused a bearer
	// (never reset: an upstream lives for one session generation at most),
	// and the earliest time for the next one.
	refused   int
	notBefore time.Time
	// The last bearer check (checkBearer), shared by concurrent 401s.
	check         sync.Mutex
	checkedBearer string
	checkedAt     time.Time
	checkedResult scienceBearerVerdict
	// signedOut holds the API paths that last answered a good bearer with
	// 401 (a signed-out account); onSignIn hears when it becomes empty or
	// not.
	signedOut map[string]bool
	onSignIn  func(needed bool)
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
	return u.remint(ctx, "", false)
}

// remint replaces stale (or an absent bearer) once; concurrent callers share
// one exchange. refused says the daemon refused stale: only those exchanges
// count against scienceMintCap.
func (u *scienceUpstream) remint(ctx context.Context, stale string, refused bool) (string, error) {
	u.mint.Lock()
	defer u.mint.Unlock()
	u.mu.Lock()
	if u.bearer != "" && u.bearer != stale {
		token := u.bearer
		u.mu.Unlock()
		return token, nil
	}
	if refused {
		if u.refused >= scienceMintCap {
			u.mu.Unlock()
			return "", errScienceRefused
		}
		u.refused++
		u.notBefore = time.Now().Add(scienceMintBackoff << (u.refused - 1))
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

// A daemon that refuses fresh credentials gets scienceMintCap exchanges,
// scienceMintBackoff apart and doubling, then the session ends. Successes in
// between do not reset the count (round-2 N5).
var scienceMintBackoff = time.Second

const scienceMintCap = 3

var errScienceRefused = errors.New("Claude Science 多次拒绝了本机渲染的访问凭证，本机渲染已停止。请回到工作台重新打开。")

// mayMint reports whether a refused bearer may be replaced now: within the
// cap and after the back-off.
func (u *scienceUpstream) mayMint() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.refused < scienceMintCap && !time.Now().Before(u.notBefore)
}

// exhausted reports whether the daemon refused scienceMintCap fresh bearers.
func (u *scienceUpstream) exhausted() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.refused >= scienceMintCap
}

type scienceBearerVerdict int

const (
	scienceBearerUnknown scienceBearerVerdict = iota
	scienceBearerGood
	scienceBearerRefused
)

// scienceBearerCheckPath answers 200 to a good bearer whether or not the
// account is signed in, and 401 to any other (science_daemon_contract_test.go
// states this for the real daemon).
const scienceBearerCheckPath = "/api/auth/status"

// scienceBearerCheckLife is how long a check's verdict serves further 401s.
const scienceBearerCheckLife = 10 * time.Second

// checkBearer asks the daemon whether token itself is refused, so that a
// 401 the app gives a signed-out account is not mistaken for a refused
// credential (round-2 N5, Q11). Concurrent callers share one check.
func (u *scienceUpstream) checkBearer(ctx context.Context, token string) scienceBearerVerdict {
	u.check.Lock()
	defer u.check.Unlock()
	u.mu.Lock()
	if u.checkedBearer == token && time.Since(u.checkedAt) < scienceBearerCheckLife {
		verdict := u.checkedResult
		u.mu.Unlock()
		return verdict
	}
	u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.origin()+scienceBearerCheckPath, nil)
	if err != nil {
		return scienceBearerUnknown
	}
	request.Host = u.hostPort()
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := u.transport.RoundTrip(request)
	if err != nil {
		return scienceBearerUnknown
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	verdict := scienceBearerUnknown
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		verdict = scienceBearerRefused
	case response.StatusCode < 300:
		verdict = scienceBearerGood
	}
	if verdict != scienceBearerUnknown {
		u.mu.Lock()
		u.checkedBearer, u.checkedAt, u.checkedResult = token, time.Now(), verdict
		u.mu.Unlock()
	}
	return verdict
}

// setSignedOut records whether path answered a good bearer with 401 and
// tells the front when the daemon starts or stops asking for a sign-in. A
// path clears only its own entry, so endpoints that work signed out do not
// hide the ones that need the account.
func (u *scienceUpstream) setSignedOut(path string, signedOut bool) {
	u.mu.Lock()
	before := len(u.signedOut) > 0
	if signedOut && len(u.signedOut) < 64 {
		if u.signedOut == nil {
			u.signedOut = map[string]bool{}
		}
		u.signedOut[path] = true
	} else if !signedOut {
		delete(u.signedOut, path)
	}
	after := len(u.signedOut) > 0
	notify := u.onSignIn
	u.mu.Unlock()
	if before != after && notify != nil {
		notify(after)
	}
}

// watchSignIn sets who hears setSignedOut.
func (u *scienceUpstream) watchSignIn(notify func(needed bool)) {
	u.mu.Lock()
	u.onSignIn = notify
	u.mu.Unlock()
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
		api := strings.HasPrefix(request.URL.Path, "/api/") && !strings.HasPrefix(request.URL.Path, "/api/auth/")
		if response.StatusCode != http.StatusUnauthorized {
			if api {
				u.setSignedOut(request.URL.Path, false)
			}
			return response, nil
		}
		// A 401 to a good bearer is the app's own answer (a signed-out
		// account): the page gets it and nothing is minted. Only a refused
		// bearer is replaced, once per request, within the cap.
		verdict := scienceBearerRefused
		if request.URL.Path != scienceBearerCheckPath {
			verdict = u.checkBearer(request.Context(), token)
		}
		if verdict == scienceBearerGood {
			if api {
				u.setSignedOut(request.URL.Path, true)
			}
			return response, nil
		}
		if verdict == scienceBearerUnknown || attempt > 0 || (request.Body != nil && request.Body != http.NoBody && request.GetBody == nil) {
			return response, nil
		}
		if u.exhausted() {
			response.Body.Close()
			return nil, errScienceRefused
		}
		if !u.mayMint() {
			return response, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if token, err = u.remint(request.Context(), token, true); err != nil {
			return nil, err
		}
	}
}

// scienceSocket audits what the browser writes into an upgraded connection.
type scienceSocket struct {
	io.ReadWriteCloser
	audit io.Writer
}

func (s scienceSocket) Write(p []byte) (int, error) { return s.audit.Write(p) }

func (s scienceSocket) CloseWrite() error {
	if stream, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return stream.CloseWrite()
	}
	return s.Close()
}

// newScienceSocketAudit wraps what the browser writes into an upgraded
// connection: every byte goes on at once and unchanged; the client frames
// (RFC 6455, masked) are unmasked chunk by chunk into a fixed buffer and
// scanned with a small carry. A hit is reported, not refused (F6), and the
// memory per socket stays fixed whatever the frame size (F7).
func newScienceSocketAudit(dst io.Writer, audit *scienceAudit, onHit func(string)) io.Writer {
	return &wsAudit{dst: dst, audit: audit, onHit: onHit, data: true}
}

type wsAudit struct {
	dst   io.Writer
	audit *scienceAudit
	onHit func(string)
	// The frame header being read, then the payload's state.
	header    [14]byte
	have      int
	remaining uint64
	key       [4]byte
	offset    uint64
	masked    bool
	data      bool
	// carry is the end of the previous chunk (a value split across chunks);
	// buffer is reused for carry plus one unmasked chunk.
	carry  []byte
	buffer []byte
}

// wsAuditChunk bounds one scan (and the buffer).
const wsAuditChunk = 32 << 10

func (w *wsAudit) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if w.audit != nil {
		w.observe(p[:n])
	}
	return n, err
}

func (w *wsAudit) observe(p []byte) {
	for len(p) > 0 {
		if w.remaining == 0 && w.have >= 0 {
			// Reading a header: 2 bytes, then the extended length, then the key.
			need := 2
			if w.have >= 2 {
				switch w.header[1] & 0x7f {
				case 126:
					need = 4
				case 127:
					need = 10
				}
				if w.header[1]&0x80 != 0 {
					need += 4
				}
			}
			if w.have < need {
				take := min(need-w.have, len(p))
				copy(w.header[w.have:], p[:take])
				w.have += take
				p = p[take:]
				continue
			}
			w.begin(need)
			continue
		}
		take := int(min(w.remaining, uint64(len(p)), wsAuditChunk))
		if w.data {
			w.scan(p[:take])
		}
		w.offset += uint64(take)
		w.remaining -= uint64(take)
		p = p[take:]
		if w.remaining == 0 {
			w.have = 0
		}
	}
}

// begin starts a frame's payload once its header (need bytes) is in.
func (w *wsAudit) begin(need int) {
	opcode := w.header[0] & 0x0f
	length := uint64(w.header[1] & 0x7f)
	switch length {
	case 126:
		length = uint64(binary.BigEndian.Uint16(w.header[2:4]))
	case 127:
		length = binary.BigEndian.Uint64(w.header[2:10])
	}
	w.masked = w.header[1]&0x80 != 0
	if w.masked {
		copy(w.key[:], w.header[need-4:need])
	}
	// Continuation frames carry on the message's kind; control frames are
	// not scanned.
	if opcode != 0 {
		w.data = opcode < 8
	}
	w.offset, w.remaining = 0, length
	if length == 0 {
		w.have = 0
		return
	}
	w.have = -1
}

func (w *wsAudit) scan(chunk []byte) {
	w.buffer = append(w.buffer[:0], w.carry...)
	start := len(w.buffer)
	w.buffer = append(w.buffer, chunk...)
	if w.masked {
		for i := range chunk {
			w.buffer[start+i] ^= w.key[(w.offset+uint64(i))%4]
		}
	}
	if category := w.audit.scan(w.buffer); category != "" {
		if w.onHit != nil {
			w.onHit(category)
		}
		// Reported once: the next scan starts after it.
		w.carry = w.carry[:0]
		return
	}
	keep := min(w.audit.window(), len(w.buffer))
	w.carry = append(w.carry[:0], w.buffer[len(w.buffer)-keep:]...)
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
