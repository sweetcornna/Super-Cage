package controlclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
)

// Conn is one controller connection, whatever carries it.
type Conn interface {
	Record() ControlFile
	Launch() LaunchResponse
	// CallContext sends a JSON request and decodes a 200 reply into output.
	// A controller that cannot be reached is a *url.Error.
	CallContext(ctx context.Context, method, path string, input, output any) (int, error)
	// SetTrustedViewCookie adds the desktop proof for requests to origin.
	SetTrustedViewCookie(origin *url.URL)
	Close()
}

// Connect proves the launcher token to the controller of profile. open reads
// the record and prepares a connection; the caller keeps the launch answer.
func Connect(profile string, open func(string) (Conn, error)) (Conn, LaunchResponse, error) {
	var launch LaunchResponse
	if LockFree(profile) {
		return nil, launch, ErrUnavailable
	}
	c, err := open(profile)
	if err != nil {
		if LockFree(profile) {
			return nil, launch, ErrUnavailable
		}
		return nil, launch, err
	}
	record := c.Record()
	status, err := c.CallContext(context.Background(), "POST", "/api/launch", map[string]string{"token": record.Token}, &launch)
	if err != nil {
		c.Close()
		var networkError *url.Error
		if errors.As(err, &networkError) {
			return nil, launch, ErrUnavailable
		}
		return nil, launch, err
	}
	if status != 200 {
		c.Close()
		if status == 503 {
			return nil, launch, ErrUnavailable
		}
		// A dead controller can leave a record whose port or PID is now reused.
		// Only a free profile lock permits a new controller to replace that
		// record. An occupied or inaccessible lock must preserve its owner.
		if LockFree(profile) {
			return nil, launch, ErrUnavailable
		}
		return nil, launch, errors.New("无法认证已有后台，未尝试终止或替换它。")
	}
	u, err := url.Parse(launch.URL)
	if err != nil || u.Scheme != "http" || u.Host != fmt.Sprintf("localhost:%d", record.Port) || u.User != nil || u.Fragment == "" {
		c.Close()
		return nil, launch, errors.New("后台返回了无效入口，未连接旧工作台。")
	}
	if launch.Controller.Protocol != 0 && launch.Controller.PID != record.PID {
		c.Close()
		return nil, launch, errors.New("后台身份已变化，请重试连接。")
	}
	return c, launch, nil
}

// Check decides whether a controller's build may be used by this client.
type Check func(ControllerIdentity) bool

// SameBuild accepts only the controller built from the same executable, as
// cage agent requires.
func SameBuild(build string) Check {
	want := ControllerIdentity{Build: build, Protocol: Protocol}
	return func(id ControllerIdentity) bool { return want.Matches(id) }
}

// CheckLocal refuses a local controller of another build or another
// configuration, before any session is opened.
func CheckLocal(launch LaunchResponse, configPath string, accepted bool) error {
	if !accepted || launch.DesktopGateway || launch.ClientOnly {
		return &AgentError{Code: "controller_mismatch", Message: "Use the CLI shipped with the running workload. No controller was replaced.", NextAction: "open_desktop"}
	}
	if filepath.Clean(launch.ConfigPath) != filepath.Clean(configPath) {
		return &AgentError{Code: "config_path_mismatch", Message: "This directory's controller uses a different configuration filename. Use its original absolute config path.", NextAction: "doctor"}
	}
	return nil
}

// Session opens an authenticated session with the launch ticket and adds the
// desktop proof.
func Session(ctx context.Context, c Conn) error {
	u, _ := url.Parse(c.Launch().URL)
	code, err := c.CallContext(ctx, "POST", "/api/session", map[string]string{"token": u.Fragment}, nil)
	if err != nil || code != 200 {
		return HTTPError(401)
	}
	c.SetTrustedViewCookie(u)
	return nil
}

// AgentConnect opens an authenticated session on the local workload.
func AgentConnect(ctx context.Context, configPath string, check Check, connect func(string) (Conn, error)) (Conn, error) {
	c, err := connect(filepath.Dir(configPath))
	if err != nil {
		return nil, &AgentError{Code: "service_unavailable", Message: "The local Super Cage controller is unavailable. Run service.start or doctor.", NextAction: "service.start"}
	}
	fail := func(err error) (Conn, error) { c.Close(); return nil, err }
	if err := CheckLocal(c.Launch(), configPath, check(c.Launch().Controller)); err != nil {
		return fail(err)
	}
	if err := Session(ctx, c); err != nil {
		return fail(err)
	}
	return c, nil
}

// AgentConnectTarget opens an authenticated session for a target: the local
// workload, or the desktop and its selected workload.
func AgentConnectTarget(ctx context.Context, configPath string, target AgentTarget, check Check, connect func(string) (Conn, error)) (Conn, error) {
	if target.Scope == "" || target.Scope == "local" {
		return AgentConnect(ctx, configPath, check, connect)
	}
	if target.Scope != "desktop" && target.Scope != "selected" && target.Scope != "client" {
		return nil, &AgentError{Code: "invalid_input", Message: "scope must be local, desktop, selected or client.", NextAction: "capabilities"}
	}
	if target.Scope == "selected" && target.Connection == "" {
		return nil, &AgentError{Code: "invalid_input", Message: "selected scope requires --connection from client.context.", NextAction: "client.context"}
	}
	path := DesktopConfigPath(configPath)
	if target.Scope == "client" {
		path = configPath
	}
	c, err := connect(filepath.Dir(path))
	if err != nil {
		return nil, &AgentError{Code: "service_unavailable", Message: "Open the Super Cage desktop before managing client configuration.", NextAction: "open_desktop"}
	}
	launch := c.Launch()
	if !check(launch.Controller) || (target.Scope != "client" && !launch.DesktopGateway) || (target.Scope == "client" && !launch.ClientOnly) || filepath.Clean(launch.ConfigPath) != filepath.Clean(path) {
		c.Close()
		return nil, &AgentError{Code: "controller_mismatch", Message: "The desktop controller does not match this CLI and configuration.", NextAction: "open_desktop"}
	}
	if err := Session(ctx, c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
