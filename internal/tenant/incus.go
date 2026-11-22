package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const instanceLabel = "user.cage-tenant.id"
const instanceSystem = "user.cage-tenant.system"

// Incus instance types the gateway can run. System containers need no
// hardware virtualization, so they also run on hosts without KVM.
const (
	InstanceVM        = "virtual-machine"
	InstanceContainer = "container"
)

type IncusConfig struct {
	Executable  string
	Project     string
	Profile     string
	Network     string
	ControlPath string
	// InstanceType is InstanceVM (the default) or InstanceContainer.
	InstanceType string
	// Vsock, for virtual machines only, reaches each workbench through a vsock
	// bridge (see incus_direct.go) instead of one incus exec per connection.
	Vsock bool
}

type IncusRuntime struct {
	cfg IncusConfig
	// locks holds one *sync.Mutex per workspace ID. It orders the gateway's
	// memory changes on an instance with its boot, see lockInstance.
	locks sync.Map
	// directSkip holds, per instance name, the time before which the vsock path
	// is not tried again after it failed.
	directSkip sync.Map
	// vsockDial and execDial replace the real dials in tests.
	vsockDial vsockStream
	execDial  func(ctx context.Context, instance string, port int) (net.Conn, error)
}

func NewIncus(cfg IncusConfig) (*IncusRuntime, error) {
	if !dnsLabel.MatchString(cfg.Project) || cfg.Project == "default" {
		return nil, errors.New("a dedicated Incus project is required")
	}
	if !dnsLabel.MatchString(cfg.Profile) || (cfg.Network != "" && !dnsLabel.MatchString(cfg.Network)) {
		return nil, errors.New("configure a named Incus profile and, if needed, one managed network")
	}
	switch cfg.InstanceType {
	case "":
		cfg.InstanceType = InstanceVM
	case InstanceVM, InstanceContainer:
	default:
		return nil, errors.New("Incus instance type must be virtual-machine or container")
	}
	if cfg.Vsock && cfg.InstanceType != InstanceVM {
		return nil, errors.New("vsock needs virtual machines: containers share the host's vsock namespace")
	}
	if !filepath.IsAbs(cfg.Executable) || !path.IsAbs(cfg.ControlPath) || path.Clean(cfg.ControlPath) != cfg.ControlPath || path.Base(cfg.ControlPath) != "control.json" || strings.ContainsAny(cfg.ControlPath, "\x00\r\n") {
		return nil, errors.New("configure an absolute Incus executable and an absolute guest control.json path")
	}
	return &IncusRuntime{cfg: cfg}, nil
}

func (r *IncusRuntime) instanceType() string {
	if r.cfg.InstanceType == "" {
		return InstanceVM
	}
	return r.cfg.InstanceType
}

func (r *IncusRuntime) command(ctx context.Context, args ...string) *exec.Cmd {
	return r.localCommand(ctx, append([]string{"--project", r.cfg.Project}, args...)...)
}

func (r *IncusRuntime) localCommand(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, r.cfg.Executable, append([]string{"--force-local"}, args...)...)
	command.WaitDelay = 2 * time.Second
	return command
}

type limitedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (output *limitedOutput) Write(data []byte) (int, error) {
	if len(data) > output.limit-output.buffer.Len() {
		return 0, errors.New("unexpected Incus output size")
	}
	return output.buffer.Write(data)
}

func (r *IncusRuntime) capture(ctx context.Context, limit int, args ...string) ([]byte, error) {
	return captureIncus(r.command(ctx, args...), limit)
}

var errIncusQuery = errors.New("Incus operation failed")

func captureIncus(command *exec.Cmd, limit int) ([]byte, error) {
	output := &limitedOutput{limit: limit}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, errIncusQuery
	}
	return output.buffer.Bytes(), nil
}

type instance struct {
	Name            string                       `json:"name"`
	Type            string                       `json:"type"`
	Status          string                       `json:"status"`
	Ephemeral       bool                         `json:"ephemeral"`
	Config          map[string]string            `json:"config"`
	Profiles        []string                     `json:"profiles"`
	ExpandedConfig  map[string]string            `json:"expanded_config"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
}

func (r *IncusRuntime) lookup(ctx context.Context, spec LaunchSpec) (*instance, error) {
	name := "cage-" + spec.VMID
	body, err := r.capture(ctx, 1<<20, "list", "local:", "--fast", "--format=json", "name="+name)
	if err != nil {
		return nil, err
	}
	var instances []instance
	if json.Unmarshal(body, &instances) != nil {
		return nil, errors.New("invalid Incus instance inventory")
	}
	for _, record := range instances {
		if record.Name != name {
			continue
		}
		system := record.Config[instanceSystem]
		if record.Type != r.instanceType() || record.Config[instanceLabel] != spec.VMID || (system != spec.SystemID && !(system == "" && spec.SystemID == defaultSystemID)) {
			return nil, errors.New("Incus instance ownership does not match the workspace")
		}
		if spec.Image != "" && record.Config["volatile.base_image"] != spec.Image {
			return nil, errors.New("Incus instance base image does not match the stored fingerprint")
		}
		return &record, nil
	}
	return nil, nil
}

func (r *IncusRuntime) Prepare(ctx context.Context, spec LaunchSpec) (*Workspace, error) {
	if !vmIDPattern.MatchString(spec.VMID) || !systemIDPattern.MatchString(spec.SystemID) {
		return nil, errors.New("invalid workspace identity")
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("Incus runtime requires a Linux host")
	}
	name := "local:cage-" + spec.VMID
	profile, err := r.loadProfile(ctx)
	if err != nil {
		return nil, err
	}
	record, err := r.lookup(ctx, spec)
	if err != nil {
		return nil, err
	}
	if record == nil {
		if !secretPattern.MatchString(spec.Image) {
			return nil, errors.New("configured Incus VM image has no pinned fingerprint")
		}
		args := []string{"init", "local:" + spec.Image, name}
		if r.instanceType() == InstanceVM {
			args = append(args, "--vm")
		}
		args = append(args, "--profile", r.cfg.Profile, "--config", instanceLabel+"="+spec.VMID, "--config", instanceSystem+"="+spec.SystemID)
		if err := r.command(ctx, args...).Run(); err != nil {
			return nil, errors.New("Incus instance creation failed")
		}
		record, err = r.lookup(ctx, spec)
		if err != nil || record == nil {
			return nil, errors.New("Incus VM could not be verified")
		}
	}
	if err := r.validateInstance(record, profile); err != nil {
		return nil, err
	}
	switch record.Status {
	case "Stopped", "Frozen":
		// Start also resumes an instance frozen by idle cooling, including one
		// frozen before a gateway restart.
		unlock := r.lockInstance(spec.VMID)
		if record.Status == "Stopped" {
			r.resetMemory(ctx, spec.VMID, record.Config)
		}
		err := r.command(ctx, "start", name).Run()
		unlock()
		if err != nil {
			return nil, errors.New("Incus instance start failed")
		}
	case "Running":
	default:
		return nil, errors.New("Incus VM is not in a startable state")
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 8*time.Second)
		workspace, err := r.connect(attempt, name)
		cancel()
		if err == nil {
			workspace.verify = func(ctx context.Context) error {
				profile, err := r.loadProfile(ctx)
				if err != nil {
					return err
				}
				record, err := r.lookup(ctx, spec)
				if err != nil {
					return err
				}
				if record == nil || record.Status != "Running" {
					return errController
				}
				return r.validateInstance(record, profile)
			}
			verification, done := context.WithTimeout(ctx, 10*time.Second)
			err := workspace.verify(verification)
			done()
			if err != nil {
				workspace.close()
				return nil, err
			}
			return workspace, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil, errController
}

// lockInstance serializes the gateway's own memory changes on one instance
// with its boot, so a resize never lands between the reset to the profile
// ceiling and the start. It returns the unlock function.
func (r *IncusRuntime) lockInstance(id string) func() {
	value, _ := r.locks.LoadOrStore(id, new(sync.Mutex))
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// resetMemory drops the instance-local limits.memory of a stopped VM, so it
// boots at the profile ceiling. Every later resize then stays within the boot
// memory, where Incus moves the balloon and never hotplugs memory that cannot
// be removed again. On failure the VM boots at its last size, which the policy
// still accepts. Caller holds the instance lock.
func (r *IncusRuntime) resetMemory(ctx context.Context, id string, local map[string]string) {
	if r.instanceType() != InstanceVM || local["limits.memory"] == "" {
		return
	}
	if err := r.command(ctx, "config", "unset", "local:cage-"+id, "limits.memory").Run(); err != nil && ctx.Err() == nil {
		log.Printf("cage-tenant: workspace %s memory reset before boot failed; it boots at its last memory size", id)
	}
}

func (r *IncusRuntime) ResolveImage(ctx context.Context, configured string) (string, error) {
	missing := errors.New("configured Incus image is unavailable")
	if !imagePattern.MatchString(configured) {
		return "", missing
	}
	if secretPattern.MatchString(configured) {
		return configured, nil
	}
	body, err := r.capture(ctx, 1<<20, "image", "list", "local:", "--format=json")
	if err != nil {
		return "", missing
	}
	var images []struct {
		Fingerprint string `json:"fingerprint"`
		Type        string `json:"type"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if json.Unmarshal(body, &images) != nil {
		return "", missing
	}
	var fingerprint string
	for _, image := range images {
		if image.Type != r.instanceType() || !secretPattern.MatchString(image.Fingerprint) {
			continue
		}
		for _, alias := range image.Aliases {
			if alias.Name == configured {
				if fingerprint != "" && fingerprint != image.Fingerprint {
					return "", missing
				}
				fingerprint = image.Fingerprint
			}
		}
	}
	if fingerprint == "" {
		return "", missing
	}
	return fingerprint, nil
}

func (r *IncusRuntime) connect(ctx context.Context, instance string) (*Workspace, error) {
	body, err := r.capture(ctx, 8192, "exec", instance, "--mode=non-interactive", "--",
		"/usr/bin/head", "-c", "8193", "--", r.cfg.ControlPath)
	if err != nil {
		return nil, errController
	}
	var control struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &control) != nil || control.Port < 1 || control.Port > 65535 || !secretPattern.MatchString(control.Token) {
		return nil, errController
	}
	dialer := &guestDialer{runtime: r, instance: instance, port: control.Port}
	workspace := newWorkspace(control.Port, dialer.dial)
	if r.directAllowed(instance) {
		if path, err := r.openDirect(ctx, workspace, instance, control.Port); err != nil {
			if errors.Is(err, errDirectUnsupported) {
				r.skipDirect(instance)
			}
			log.Printf("cage-tenant: workspace %s uses incus exec for now; the vsock path is unavailable: %v", instance, err)
		} else {
			dialer.direct.Store(path)
			log.Printf("cage-tenant: workspace %s dials its workbench through vsock (CID %d, port %d)", instance, path.cid, path.port)
		}
	}
	err = workspace.bootstrap(ctx, control.Token)
	if path := dialer.direct.Load(); err != nil && path != nil {
		// The handshake failed over vsock. The launcher token can be used again,
		// so repeat it over exec: only if that works was it the path.
		dialer.direct.Store(nil)
		workspace.control.CloseIdleConnections()
		if err = workspace.bootstrap(ctx, control.Token); err == nil {
			r.skipDirect(instance)
			path.fail(errors.New("the workspace handshake worked over exec but not over vsock"))
		}
	}
	if err != nil {
		workspace.close()
		return nil, err
	}
	return workspace, nil
}

func (r *IncusRuntime) dial(ctx context.Context, instance string, port int) (net.Conn, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	guestInput, hostOutput, err := os.Pipe()
	if err != nil {
		return nil, errController
	}
	hostInput, guestOutput, err := os.Pipe()
	if err != nil {
		guestInput.Close()
		hostOutput.Close()
		return nil, errController
	}
	defer guestInput.Close()
	defer guestOutput.Close()
	command := r.command(context.WithoutCancel(ctx), "exec", instance, "--mode=non-interactive", "--",
		"/usr/bin/socat", "STDIO", fmt.Sprintf("TCP4:127.0.0.1:%d,connect-timeout=5", port))
	command.Stdin, command.Stdout = guestInput, guestOutput
	if err := command.Start(); err != nil {
		hostInput.Close()
		hostOutput.Close()
		return nil, errController
	}
	go func() { _ = command.Wait() }()
	conn := &agentConn{input: hostInput, output: hostOutput, process: command.Process, port: port}
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}
	return conn, nil
}

type agentConn struct {
	input, output *os.File
	process       *os.Process
	port          int
	once          sync.Once
}

func (c *agentConn) Read(data []byte) (int, error)  { return c.input.Read(data) }
func (c *agentConn) Write(data []byte) (int, error) { return c.output.Write(data) }
func (c *agentConn) Close() error {
	c.once.Do(func() {
		_ = c.process.Kill()
		_ = c.input.Close()
		_ = c.output.Close()
	})
	return nil
}
func (*agentConn) LocalAddr() net.Addr { return &net.UnixAddr{Name: "incus-guest-agent", Net: "unix"} }
func (c *agentConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: c.port}
}
func (c *agentConn) SetReadDeadline(deadline time.Time) error {
	return c.input.SetReadDeadline(deadline)
}
func (c *agentConn) SetWriteDeadline(deadline time.Time) error {
	return c.output.SetWriteDeadline(deadline)
}
func (c *agentConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.SetReadDeadline(deadline), c.SetWriteDeadline(deadline))
}
