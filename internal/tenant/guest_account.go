package tenant

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxGuestAccountFile = 4 << 10
	maxGuestAccountBody = 8 << 10
	guestAccountTimeout = 5 * time.Second

	defaultGuestAccountUsername = "cage"
	guestAccountStateFile       = "guest-account.json"
)

var errGuestAccountUnavailable = errors.New("guest account setup is unavailable")

// GuestAccount is an explicitly configured account for tenant VM workspaces.
// The gateway creates it once on a blank VM and unlocks it on later visits so
// a portal-authenticated owner does not need a second per-VM password. The
// password is derived per VM from the configured secret; the secret never
// enters the portal, browser, or tenant state file.
type GuestAccount struct {
	username string
	secret   []byte
}

type guestAccountFile struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

func LoadGuestAccount(filename string) (*GuestAccount, error) {
	if filename == "" {
		return nil, errors.New("a guest account file is required")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("guest account file is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGuestAccountFile {
		return nil, errors.New("guest account file must be a regular JSON file under 4 KiB")
	}
	if !privateFile(info) {
		return nil, errors.New("guest account file must be private (0600)")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxGuestAccountFile+1))
	decoder.DisallowUnknownFields()
	var input guestAccountFile
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("guest account file must contain username and secret")
	}
	if !validGuestAccountName(input.Username) || !secretPattern.MatchString(input.Secret) {
		return nil, errors.New("guest account file needs a valid username and a 64-character hex secret")
	}
	secret, err := hex.DecodeString(input.Secret)
	if err != nil {
		return nil, errors.New("guest account secret is invalid")
	}
	return &GuestAccount{username: input.Username, secret: secret}, nil
}

// loadOrCreateGuestAccount keeps the managed account enabled for new blank VM
// deployments without requiring an extra administrator step. The secret lives
// beside the tenant state, is private, and is never written to state.json.
// An explicitly configured file still wins before this function is called.
func loadOrCreateGuestAccount(stateDir string) (*GuestAccount, error) {
	path := filepath.Join(stateDir, guestAccountStateFile)
	if _, err := os.Lstat(path); err == nil {
		return LoadGuestAccount(path)
	} else if !os.IsNotExist(err) {
		return nil, errors.New("guest account secret in the state directory is unavailable")
	}
	// NewServer holds the exclusive state directory lock. Write and sync the
	// complete record before publishing it, as for the tenant database.
	dir, err := os.Open(stateDir)
	if err != nil {
		return nil, errors.New("cannot open the guest account state directory")
	}
	defer dir.Close()
	file, err := os.CreateTemp(stateDir, ".guest-account-*")
	if err != nil {
		return nil, errors.New("cannot create the guest account secret in the state directory")
	}
	defer os.Remove(file.Name())
	secret := hex.EncodeToString(randomBytes(32))
	err = json.NewEncoder(file).Encode(guestAccountFile{Username: defaultGuestAccountUsername, Secret: secret})
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err == nil {
		err = syncDirectory(dir)
	}
	if err != nil {
		return nil, errors.New("cannot persist the guest account secret in the state directory")
	}
	return LoadGuestAccount(path)
}

func validGuestAccountName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && utf8.RuneCountInString(name) <= 40 &&
		strings.IndexFunc(name, unicode.IsControl) == -1
}

// password derives a unique password for one VM. A compromised VM password
// therefore does not unlock another VM, and the configured secret is not
// stored with the VM record.
func (a *GuestAccount) password(vmID string) []byte {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("cage-tenant guest account v1\x00"))
	mac.Write([]byte(vmID))
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

type guestAccountState struct {
	Exists                bool   `json:"exists"`
	Unlocked              bool   `json:"unlocked"`
	Username              string `json:"username"`
	RequireAuthentication *bool  `json:"require_authentication"`
}

// ensureGuestAccount enters the guest workbench account. It returns false
// without an error when the normal account screen should remain available.
// Call before publishing the workspace to browsers.
func (w *Workspace) ensureGuestAccount(ctx context.Context, account *GuestAccount, vmID string, previous *Workspace) (bool, error) {
	state, status, err := w.guestAccountState(ctx)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return false, nil
	default:
		return false, errGuestAccountUnavailable
	}
	if state.Unlocked {
		return true, nil
	}
	// Bootstrap cookies are random per controller process. Reconnecting to
	// the same controller must not reverse a lock or suspend in that process.
	if previous != nil && !w.replacedController(previous) {
		return false, nil
	}
	if state.Exists && !state.allowsManagedResume(account.username) {
		return false, nil
	}
	password := account.password(vmID)
	defer clear(password)
	if !state.Exists {
		status, err = w.guestAccountAction(ctx, "create", map[string]string{
			"username": account.username, "password": string(password), "confirm_password": string(password),
		})
		if err != nil {
			return false, err
		}
		if status == http.StatusOK {
			return true, nil
		}
		if status != http.StatusBadRequest {
			return false, errGuestAccountUnavailable
		}
		// A concurrent first visit may have created the account. Treat it as
		// an existing account rather than overwriting anything.
		state, status, err = w.guestAccountState(ctx)
		if err != nil {
			return false, err
		}
		if status != http.StatusOK || !state.Exists {
			return false, errGuestAccountUnavailable
		}
		if state.Unlocked {
			return true, nil
		}
		if !state.allowsManagedResume(account.username) {
			return false, nil
		}
	}
	status, err = w.guestAccountAction(ctx, "unlock", map[string]string{"password": string(password)})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusBadRequest, http.StatusUnauthorized:
		// The upstream workbench reports a wrong password as 400 (and may use
		// 401). Leave an existing account and its encrypted data untouched.
		return false, nil
	default:
		return false, errGuestAccountUnavailable
	}
}

// A newer session has a different cookie, issued only after the launcher
// handshake and a control-plane request. Missing evidence cannot prove a restart.
func (w *Workspace) replacedController(previous *Workspace) bool {
	return w.cookie != nil && previous.cookie != nil && w.cookie.Value != previous.cookie.Value
}

func (state guestAccountState) allowsManagedResume(username string) bool {
	// The workbench persists this bit on a manual lock, logout or credential
	// recovery. Require an explicit false from the upstream API for a saved
	// account. The derived password must still pass normal authentication.
	return state.Username == username && state.RequireAuthentication != nil && !*state.RequireAuthentication
}

func (w *Workspace) guestAccountState(ctx context.Context) (guestAccountState, int, error) {
	status, body, err := w.guestAccountRequest(ctx, http.MethodGet, "/api/account", nil)
	if err != nil {
		return guestAccountState{}, 0, err
	}
	var state guestAccountState
	if status == http.StatusOK && (json.Unmarshal(body, &state) != nil || state.Username == "" && state.Exists) {
		return guestAccountState{}, 0, errGuestAccountUnavailable
	}
	return state, status, nil
}

func (w *Workspace) guestAccountAction(ctx context.Context, action string, input any) (int, error) {
	status, body, err := w.guestAccountRequest(ctx, http.MethodPost, "/api/account/"+action, input)
	// A create response carries a recovery key. The gateway has no use for it
	// and must not keep it around or forward it to the browser.
	clear(body)
	return status, err
}

func (w *Workspace) guestAccountRequest(ctx context.Context, method, path string, input any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, guestAccountTimeout)
	defer cancel()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, nil, errGuestAccountUnavailable
		}
		defer clear(data)
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, w.origin.String()+path, body)
	if err != nil {
		return 0, nil, errGuestAccountUnavailable
	}
	request.Header.Set("Origin", w.origin.String())
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if w.cookie != nil {
		request.AddCookie(w.cookie)
	}
	response, err := w.control.RoundTrip(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGuestAccountBody+1))
	if err != nil || len(data) > maxGuestAccountBody {
		return 0, nil, errGuestAccountUnavailable
	}
	return response.StatusCode, data, nil
}
