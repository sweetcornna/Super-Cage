// Package controlclient is the client half of the local control transport:
// a controller's private record and lock, the launcher token proof, the
// controller checks and the session, shared by internal/workbench, which
// carries them over net/http, and the slim cage-attach program, which uses
// the minimal client in internal/controlwire. It must never import net/http,
// gorilla/websocket or internal/workbench: each of them alone costs the slim
// program megabytes of resident memory (see cmd/cage-attach).
package controlclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

// Protocol is the control protocol a controller and its clients speak.
const Protocol = 1

// CookieName is the base name of the workbench session cookie.
const CookieName = "cage_session"

// ConnectionHeader names the connection generation a request belongs to.
const ConnectionHeader = "X-Cage-Connection"

// RequestTimeout bounds ordinary launcher requests, which only read
// controller memory.
const RequestTimeout = 5 * time.Second

// ControllerIdentity is what a controller says about its own build.
type ControllerIdentity struct {
	Version  string `json:"version"`
	Build    string `json:"build"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	// Stamp is the build stamp linked into this controller (version.Stamp).
	Stamp string `json:"stamp,omitempty"`
	// Attach is the cage-attach installed beside this controller, as it
	// verified it; absent when there is none.
	Attach *Companion `json:"attach,omitempty"`
}

// Matches reports whether two identities are the same executable build.
func (identity ControllerIdentity) Matches(other ControllerIdentity) bool {
	return identity.Build != "" && identity.Build == other.Build && identity.Protocol == other.Protocol
}

// ControlFile is the private record a running controller publishes.
type ControlFile struct {
	Port       int                `json:"port"`
	Token      string             `json:"token"`
	PID        int                `json:"pid"`
	Controller ControllerIdentity `json:"controller"`
}

// LaunchResponse is the controller's answer to the launcher token.
type LaunchResponse struct {
	ConfigPath     string             `json:"config_path,omitempty"`
	ClientOnly     bool               `json:"client_only,omitempty"`
	DesktopGateway bool               `json:"desktop_gateway,omitempty"`
	URL            string             `json:"url"`
	Controller     ControllerIdentity `json:"controller"`
}

// ErrUnavailable means no controller serves this profile.
var ErrUnavailable = errors.New("control service is unavailable")

// TrustedViewProof is the desktop proof derived from the launcher token.
// Only the local launcher possesses the token; its independent cookie jar
// carries this proof, and the browser and SSH tickets never receive it.
func TrustedViewProof(token string) string {
	digest := sha256.Sum256([]byte("trusted desktop view\x00" + token))
	return hex.EncodeToString(digest[:])
}

// TrustedViewCookie is the name of the cookie that carries the desktop proof
// for a controller on port.
func TrustedViewCookie(port string) string { return CookieName + "_" + port + "-desktop" }

// DesktopConfigPath is where the desktop entry keeps its own controller. The
// entry and the local workload must never share a controller lock, shutdown
// hook or service cookie, even with an explicitly selected profile.
func DesktopConfigPath(fullConfig string) string {
	return filepath.Join(filepath.Dir(fullConfig), "desktop-entry", "config.json")
}

// Origin is the origin a controller serves on, as browsers and the launcher
// name it.
func Origin(record ControlFile) string { return fmt.Sprintf("http://localhost:%d", record.Port) }

// ReadRecord reads a controller's private record without contacting it.
func ReadRecord(profile string) (ControlFile, error) {
	var record ControlFile
	path := filepath.Join(profile, "control.json")
	if err := host.CheckPrivateFile(path); err != nil {
		if os.IsNotExist(err) {
			return record, ErrUnavailable
		}
		return record, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return record, ErrUnavailable
	}
	if err != nil {
		return record, err
	}
	if json.Unmarshal(b, &record) != nil || record.Port < 1024 || record.Port > 65535 || len(record.Token) != 64 || record.PID < 1 {
		return record, errors.New("后台记录无效，请保留现有环境并检查私有配置。")
	}
	return record, nil
}
