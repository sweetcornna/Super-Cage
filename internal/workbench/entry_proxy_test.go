//go:build !windows

package workbench

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/version"
)

// The proxy reads what the entry writes under the same names.
func TestEntryProxySharesTheEntrysNames(t *testing.T) {
	if entryproxy.LocalVMDir != cage.LocalVMDir || entryproxy.DefaultEnvironment != defaultLocalVMID ||
		entryproxy.ClientDir != entryClientDir || entryproxy.DeviceRecord != entryDeviceRecord || entryproxy.DeviceKey != entryDeviceKey {
		t.Fatal("names differ")
	}
	for _, id := range []string{"", "0123456", "01234567", "0123456789abcdef", "0123456789ABCDEF", strings.Repeat("a", 64), strings.Repeat("a", 65), "g1234567", "../vms/x"} {
		if entryproxy.ValidEnvironment(id) != validLocalVMID(id) {
			t.Errorf("%q: proxy %v, entry %v", id, entryproxy.ValidEnvironment(id), validLocalVMID(id))
		}
	}
	paths := entryPaths{SSHDir: t.TempDir()}
	if (entryproxy.Paths{SSHDir: paths.SSHDir}).KnownHosts("default") != paths.knownHosts("default") {
		t.Fatal("known_hosts paths differ")
	}
}

// The Host block names the slim cage-attach when it is installed beside
// cage, and cage otherwise.
func TestEntryProxyCommandPrefersTheSlimProgram(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "cage")
	if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := entryProxyCommand(full, "default"); got != shellescape.Quote(full)+" entry proxy default" {
		t.Fatalf("without cage-attach: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "cage-attach"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := entryProxyCommand(full, "0123456789abcdef"); got != shellescape.Quote(filepath.Join(dir, "cage-attach"))+" entry proxy 0123456789abcdef" {
		t.Fatalf("with cage-attach: %q", got)
	}
}

// TestEntryProxyFootprint measures the resident memory of the ProxyCommand
// process an ssh connection keeps: idle, while and after relaying 64 MB,
// and over four connections at once, with the idle CPU time. It runs the
// built program CAGE_ENTRY_PROXY_BINARY (a cage-attach built with the stamp
// CAGE_ATTACH_STAMP, or a cage) as the generated Host block's ProxyCommand
// under the system ssh, with HOME in the test directory. CAGE_ENTRY_BENCH=1
// runs it.
func TestEntryProxyFootprint(t *testing.T) {
	program := os.Getenv("CAGE_ENTRY_PROXY_BINARY")
	if os.Getenv("CAGE_ENTRY_BENCH") == "" || program == "" {
		t.Skip("set CAGE_ENTRY_BENCH=1 and CAGE_ENTRY_PROXY_BINARY")
	}
	// cage hands cage entry proxy to a cage-attach beside it (an older
	// Host block that names cage).
	slim := filepath.Base(program) == "cage-attach" || attach.Beside(program) != ""
	if stamp := os.Getenv("CAGE_ATTACH_STAMP"); slim && stamp != "" {
		previous := version.Stamp
		version.Stamp = stamp
		t.Cleanup(func() { version.Stamp = previous })
	}
	f := newEntryFixture(t, true)
	// The program finds the device key and the pin where it would for this
	// HOME; the fixture writes them there.
	client := filepath.Join(f.dir, "client")
	f.paths.SSHDir = filepath.Join(client, ".ssh")
	if runtime.GOOS == "darwin" {
		f.paths.DeviceDir = filepath.Join(client, "Library", "Application Support", "Super Cage", "entry")
	} else {
		f.paths.DeviceDir = filepath.Join(client, ".local", "share", "super-cage", "entry")
	}
	f.paths.Proxy = func(environment string) string { return entryQuote(program) + " entry proxy " + environment }
	f.on()
	if slim {
		// The controller verifies the cage-attach beside its executable.
		f.s.exe = filepath.Join(filepath.Dir(program), "cage")
	} else {
		// cage accepts only the controller of its very build; from here on
		// this test's own agent calls would not.
		data, err := os.ReadFile(program)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		f.s.identity.Build = hex.EncodeToString(sum[:])
	}
	ssh := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, sshExecutable(), append([]string{"-F", f.paths.clientFile()}, args...)...)
		cmd.Env = f.sshEnv(cage.ConfigEnv + "=" + f.s.configPath)
		cmd.Dir = f.dir
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// proxyOf is the ProxyCommand process of an ssh this test started.
	proxyOf := func(cmd *exec.Cmd) int {
		var pid int
		entryAwait(t, 10*time.Second, "the proxy process", func() bool {
			out, _ := exec.Command("/usr/bin/pgrep", "-P", strconv.Itoa(cmd.Process.Pid)).Output()
			pid, _ = strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
			return pid != 0
		})
		return pid
	}
	open := func() (*exec.Cmd, int) {
		cmd := ssh(ctx, "cage-default", "echo ready; exec sleep 120")
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || line != "ready\n" {
			t.Fatalf("no connection: %q %v", line, err)
		}
		return cmd, proxyOf(cmd)
	}
	cpu := func(pid int) string {
		out, _ := exec.Command("/bin/ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
		return strings.TrimSpace(string(out))
	}
	started := time.Now()
	_, pid := open()
	comm, _ := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	t.Logf("%s: connection up in %v; the proxy process runs %s", filepath.Base(program), time.Since(started).Round(time.Millisecond), strings.TrimSpace(string(comm)))
	time.Sleep(2 * time.Second)
	idle, idleFootprint := processRSS(t, pid), processFootprint(pid)
	cpu0 := cpu(pid)
	time.Sleep(10 * time.Second)
	t.Logf("idle: RSS %d KB, footprint %s; CPU time %s -> %s over 10 s idle", idle, idleFootprint, cpu0, cpu(pid))
	// 64 MB relayed from the guest, sampled as it flows.
	bulk := ssh(ctx, "cage-default", "head -c 67108864 /dev/zero")
	out, _ := bulk.StdoutPipe()
	bulk.Stderr = io.Discard
	if err := bulk.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bulk.Process.Kill(); _ = bulk.Wait() })
	began := time.Now()
	bulkPID := 0
	peak := 0
	done := make(chan int64, 1)
	go func() { n, _ := io.Copy(io.Discard, out); done <- n }()
	bulkPID = proxyOf(bulk)
	var n int64
	for waiting := true; waiting; {
		select {
		case n = <-done:
			waiting = false
		case <-time.After(50 * time.Millisecond):
			if out, err := exec.Command("/bin/ps", "-o", "rss=", "-p", strconv.Itoa(bulkPID)).Output(); err == nil {
				if kb, _ := strconv.Atoi(strings.TrimSpace(string(out))); kb > peak {
					peak = kb
				}
			}
		}
	}
	elapsed := time.Since(began)
	if n != 64<<20 {
		t.Fatalf("relayed %d bytes", n)
	}
	t.Logf("64 MB relayed in %v (%.0f MB/s), proxy RSS peak sampled %d KB", elapsed.Round(time.Millisecond), 64/elapsed.Seconds(), peak)
	_ = bulk.Wait()
	// Four connections at once: one proxy each.
	var total int
	var each []string
	for range 4 {
		_, p := open()
		kb := processRSS(t, p)
		total += kb
		each = append(each, strconv.Itoa(kb))
	}
	t.Logf("4 more connections: proxy RSS %s KB, %d KB together", strings.Join(each, ", "), total)
}
