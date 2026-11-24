package workbench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// controlAuth is the version of the controller proof that control.json
// announces. A client reads it from the private record, never from the
// network, so an impostor cannot talk a client down to an older protocol.
// The proof and its errors are controlclient's, shared with cage-attach.
const controlAuth = controlclient.ProofVersion

const controllerProofPath = controlclient.ProofPath

var (
	errControllerUnproven = controlclient.ErrControllerUnproven
	errControllerOutdated = controlclient.ErrControllerOutdated
	errClientOutdated     = controlclient.ErrClientOutdated
)

var proofSecret = regexp.MustCompile("^[0-9a-f]{64}$")

// controllerIdentityError reports the errors whose message the user must see
// as is (an update is needed, or the server did not prove itself) rather
// than as an unavailable controller.
func controllerIdentityError(err error) bool { return controlclient.Identity(err) != nil }

func (s *Server) controlRecord(port int) controlFile {
	return controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: s.identity, Auth: controlAuth, ProofKey: s.proofKey}
}

// controllerProofMAC binds an answer to one TCP connection: the client's and
// the server's address as each side sees them. A server that relays the
// challenge to the real controller over its own connection gets an answer
// for different addresses, which the client rejects.
func controllerProofMAC(key, host, client, server, nonce string) string {
	return controlclient.ProofMAC(key, host, client, server, nonce)
}

// controllerProof answers a client's challenge before the client sends any
// credential. The middleware serves it on this listener only, in every mode,
// before the session check, workload routing and the account lock.
func (s *Server) controllerProof(w http.ResponseWriter, r *http.Request) {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if r.Method != http.MethodPost || s.proofKey == "" || local == nil {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Version int    `json:"version"`
		Nonce   string `json:"nonce"`
	}
	if !readJSON(w, r, &in, 1<<10) {
		return
	}
	if in.Version != controlAuth || !proofSecret.MatchString(in.Nonce) {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	jsonReply(w, map[string]any{"version": controlAuth, "proof": controllerProofMAC(s.proofKey, r.Host, r.RemoteAddr, local.String(), in.Nonce)})
}

// controllerDial reaches one profile's controller. It never resolves a name:
// "localhost" can resolve to ::1 first, where another local user may listen
// on the same port. Each new connection must first prove that the server holds
// the record's proof key, before net/http writes a request on it, so the
// launcher token, tickets and cookies reach only this profile's controller.
//
// Residual (A2 of the review of controller-dial-hardening, documented, not
// fixed): the desktop windows' web views load http://localhost:<port>
// themselves and prove nothing. Only the [::1] twin that the controller
// holds (listenLoopback) keeps another local user's listener from those
// loads, and only while the controller lives.
func controllerDial(record controlFile) func(context.Context, string, string) (net.Conn, error) {
	host := fmt.Sprintf("localhost:%d", record.Port)
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(record.Port))
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != host {
			return nil, errors.New("拒绝连接本机后台以外的地址")
		}
		if record.ProofKey == "" {
			return nil, errControllerOutdated
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp4", target)
		if err != nil {
			return nil, err
		}
		if err = proveController(ctx, conn, record.ProofKey, host); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// proveController runs the challenge on a new connection. A connection that
// fails reads as an unavailable controller; a wrong answer is
// errControllerUnproven. Either way the connection is never used.
func proveController(ctx context.Context, conn net.Conn, key, host string) error {
	deadline := time.Now().Add(5 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	nonce := randomToken()
	body, _ := json.Marshal(map[string]any{"version": controlAuth, "nonce": nonce})
	request, _ := http.NewRequest(http.MethodPost, "http://"+host+controllerProofPath, bytes.NewReader(body))
	request.Header.Set("Origin", "http://"+host)
	request.Header.Set("Content-Type", "application/json")
	if err := request.Write(conn); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<10+1))
	response.Body.Close()
	if err != nil {
		return err
	}
	if !stop() {
		return ctx.Err()
	}
	var reply struct {
		Version int    `json:"version"`
		Proof   string `json:"proof"`
	}
	want := controllerProofMAC(key, host, conn.LocalAddr().String(), conn.RemoteAddr().String(), nonce)
	// The connection is handed on only if the server keeps it open and
	// nothing else is waiting in it.
	if response.StatusCode != http.StatusOK || response.Close || len(data) > 1<<10 || reader.Buffered() != 0 ||
		json.Unmarshal(data, &reply) != nil || reply.Version != controlAuth || !hmac.Equal([]byte(reply.Proof), []byte(want)) {
		return errControllerUnproven
	}
	return conn.SetDeadline(time.Time{})
}
