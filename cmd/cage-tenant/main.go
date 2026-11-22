package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/sylvinus/agent-vm/internal/tenant"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	var config tenant.Config
	var incus tenant.IncusConfig
	var listen, uiDir, image, systemsFile, guestAccountFile, instanceType, memoryReserve, memoryMin string
	var busyPercent float64
	flag.StringVar(&listen, "listen", "127.0.0.1:8080", "loopback HTTP listen address")
	flag.StringVar(&config.Origin, "origin", "", "public portal origin, without a trailing slash")
	flag.BoolVar(&config.Development, "dev", false, "enable HTTP development on cage.localhost")
	flag.BoolVar(&config.EnablePreviewRegistration, "enable-preview-registration", false, "enable self-service registration for a trusted preview audience only")
	flag.StringVar(&config.StateDir, "state-dir", ".cage-tenant", "private account and VM state directory")
	flag.StringVar(&uiDir, "ui-dir", "ui/tenant", "directory containing the three portal assets")
	flag.IntVar(&config.AccountVMLimit, "account-vm-limit", 5, "maximum VM records per account")
	flag.IntVar(&config.TotalVMLimit, "total-vm-limit", 20, "maximum VM records across all accounts")
	flag.IntVar(&config.ParallelStarts, "parallel-starts", 2, "maximum simultaneous gateway VM preparation and recheck tasks")
	flag.BoolVar(&config.TrustLoopbackClientIP, "trust-loopback-client-ip", false, "trust one X-Real-IP value from the loopback TLS proxy for rate limits")
	flag.StringVar(&incus.Executable, "incus-bin", "/usr/bin/incus", "trusted absolute Incus CLI path")
	flag.StringVar(&incus.Project, "incus-project", "cage-tenant", "dedicated local Incus project")
	flag.StringVar(&incus.Profile, "incus-profile", "default", "trusted profile within the dedicated Incus project")
	flag.StringVar(&incus.Network, "incus-network", "", "allow one NIC on this trusted managed Incus network (default: no NIC)")
	flag.StringVar(&image, "incus-image", "", "local Debian 12 VM fingerprint or alias for single-image deployments")
	flag.StringVar(&systemsFile, "systems-file", "", "trusted JSON catalog of systems and full local Incus VM fingerprints")
	flag.StringVar(&guestAccountFile, "guest-account-file", "", "private JSON ({username, secret}) used to enter tenant VM workbenches without a second prompt")
	flag.StringVar(&config.LegacyDebianImage, "legacy-debian-image", "", "verified original Debian VM fingerprint for migrating legacy state (not a catalog alias)")
	flag.StringVar(&incus.ControlPath, "guest-control-path", "/home/cage/.claude-cage/control.json", "absolute control.json path inside every VM")
	flag.StringVar(&instanceType, "incus-instance-type", "vm", "vm (needs KVM) or container (Incus system containers; no hardware virtualization needed)")
	flag.BoolVar(&incus.Vsock, "incus-vsock", false, "VM mode only: reach each workbench through a vsock bridge (one long-lived \"cage vsock-relay\" per workspace, started through incus exec, authenticated per connection with a secret only this gateway process and that relay know) instead of one incus exec per connection; falls back to incus exec when the bridge cannot be used")
	flag.DurationVar(&config.FreezeAfter, "freeze-after", 15*time.Minute, "freeze a workspace after this much idle time; its memory moves to swap and it resumes on the next request (0 disables idle cooling)")
	flag.DurationVar(&config.StopAfter, "stop-after", 72*time.Hour, "shut a frozen workspace down after this much idle time, keeping its disk (0 keeps it frozen)")
	flag.Float64Var(&busyPercent, "idle-cpu-percent", 5, "guest CPU use, in percent of one CPU, that counts as activity (0 ignores guest CPU)")
	flag.DurationVar(&config.MemoryInterval, "memory-interval", 15*time.Second, "resize running VMs to this host's memory this often; the profile limits.memory is each VM's ceiling (0 disables elastic memory; VM mode only)")
	flag.StringVar(&memoryReserve, "memory-reserve", "", "host memory kept for the host itself, such as 4GiB (default: the larger of 2GiB and 15% of host memory); below twice this much available, VMs give back their caches")
	flag.StringVar(&memoryMin, "memory-min", "2GiB", "smallest memory size elastic memory gives a running VM (at least 1GiB)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch instanceType {
	case "vm":
		incus.InstanceType = tenant.InstanceVM
	case "container":
		incus.InstanceType = tenant.InstanceContainer
	default:
		return errors.New("incus-instance-type must be vm or container")
	}
	config.BusyCPU = busyPercent / 100
	var valid bool
	if memoryReserve != "" {
		if config.MemoryReserve, valid = tenant.ParseMemorySize(memoryReserve); !valid {
			return errors.New("memory-reserve must be a memory size such as 4GiB")
		}
	}
	if config.MemoryMin, valid = tenant.ParseMemorySize(memoryMin); !valid {
		return errors.New("memory-min must be a memory size such as 2GiB")
	}
	host, port, err := net.SplitHostPort(listen)
	address := net.ParseIP(host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || address == nil || !address.IsLoopback() || portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return errors.New("listen must be a numeric loopback IP and a fixed port")
	}
	if config.Development {
		expected := "http://cage.localhost:" + port
		if config.Origin != "" && config.Origin != expected {
			return errors.New("development origin must match http://cage.localhost:<listen port>")
		}
		config.Origin = expected
		if config.TrustLoopbackClientIP {
			return errors.New("client IP forwarding is disabled in development")
		}
	}
	if runtime.GOOS != "linux" && !config.Development {
		return errors.New("production requires a Linux Incus host")
	}
	if runtime.GOOS == "linux" {
		if info, err := os.Stat(incus.Executable); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("configured Incus CLI is not executable")
		}
	}
	incusRuntime, err := tenant.NewIncus(incus)
	if err != nil {
		return err
	}
	config.Runtime = incusRuntime
	switch {
	case systemsFile != "" && image != "":
		return errors.New("choose either -systems-file or -incus-image")
	case systemsFile != "":
		config.Systems, err = tenant.LoadSystems(systemsFile)
		if err != nil {
			return err
		}
	case image != "":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fingerprint, err := incusRuntime.ResolveImage(ctx, image)
		cancel()
		if err != nil {
			return err
		}
		config.Systems = []tenant.System{tenant.DefaultSystem(fingerprint)}
	}
	if guestAccountFile != "" {
		config.GuestAccount, err = tenant.LoadGuestAccount(guestAccountFile)
		if err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(uiDir)
	if err != nil {
		return fmt.Errorf("open portal UI: %w", err)
	}
	defer root.Close()
	config.UI = root.FS()
	for _, name := range []string{"index.html", "app.mjs", "styles.css"} {
		if info, err := fs.Stat(config.UI, name); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("portal UI asset unavailable: %s", name)
		}
	}
	gateway, err := tenant.NewServer(config)
	if err != nil {
		return err
	}
	defer gateway.Close()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: gateway, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0),
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-signals.Done()
		gateway.Close()
		_ = server.Close()
	}()
	log.Printf("cage-tenant listening on %s for %s", listen, config.Origin)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
