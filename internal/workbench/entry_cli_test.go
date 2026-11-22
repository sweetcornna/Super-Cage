//go:build !windows

package workbench

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Task item 4: cage entry against a running controller. stdin is never a
// terminal here, so cage entry on and include do not ask about the Include
// line and write nothing to ~/.ssh/config. proxy and ssh are not run
// in-process: EntryMain derives the real home's paths for them; the system
// ssh tests run the same proxy code with test paths (TestEntryProxyHelper).
func TestEntryCLI(t *testing.T) {
	f := newEntryFixture(t, true)
	run := func(args ...string) (string, string, int) {
		t.Helper()
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer devnull.Close()
		var stdout, stderr bytes.Buffer
		code := EntryMain(args, f.s.configPath, devnull, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	if out, _, code := run("status"); code != 0 || out != "本机终端（默认环境）：未开启\n" {
		t.Fatalf("status: %d %q", code, out)
	}
	if _, stderr, code := run("off"); code != 1 || stderr != "Super Cage："+errEntryOff.Message+"\n" {
		t.Fatalf("off while off: %d %q", code, stderr)
	}
	// cage entry on needs the person at a terminal; no option skips it
	// (TestEntryOnAsksThePerson asks on a pty).
	for _, args := range [][]string{{"on"}, {"on", "--json"}, {"on", "default"}, {"on", "--yes"}, {"on", "-y"}, {"on", "--force"}} {
		out, stderr, code := run(args...)
		if code == 0 || out != "" || !strings.HasPrefix(stderr, "Super Cage：") {
			t.Fatalf("%q without a terminal: %d %q %q", args, code, out, stderr)
		}
		if status, _, _ := run("status"); status != "本机终端（默认环境）：未开启\n" {
			t.Fatalf("%q without a terminal turned the entry on: %q", args, status)
		}
	}
	if _, stderr, _ := run("on"); stderr != "Super Cage："+entryOnRefusal+"\n" {
		t.Fatalf("on without a terminal: %q", stderr)
	}
	// The window turns it on as before.
	f.on()
	out, stderr, code := run("status")
	if code != 0 || !strings.Contains(out, "已开启 · 没有连接") || !strings.Contains(out, "任何终端：ssh cage-default") {
		t.Fatalf("status after on: %d %q %q", code, out, stderr)
	}
	if _, err := os.Stat(f.paths.userConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("turning the entry on wrote ~/.ssh/config")
	}
	out, _, code = run("status", "--json")
	var view entryStatusView
	if code != 0 || json.Unmarshal([]byte(out), &view) != nil || !view.Enabled || view.HostAlias != "cage-default" || view.Commands == nil {
		t.Fatalf("status --json: %d %q", code, out)
	}
	// Herdr is parked: nothing cage entry prints names it.
	if _, usage, _ := run(); strings.Contains(strings.ToLower(out+usage), "herdr") {
		t.Fatalf("cage entry names Herdr: %q %q", out, usage)
	}
	if _, _, code := run("rename", f.computer.ID, "工作本"); code != 0 {
		t.Fatal("rename", code)
	}
	if out, _, code := run("devices"); code != 0 || !strings.Contains(out, f.computer.ID[:8]+"  工作本  SHA256:") || !strings.Contains(out, "从未使用 · 这台电脑") {
		t.Fatalf("devices: %d %q", code, out)
	}
	if out, _, code := run("include"); code != 0 || !strings.HasPrefix(out, "未修改 ") {
		t.Fatalf("include without a terminal: %d %q", code, out)
	}
	if _, err := os.Stat(f.paths.userConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cage entry include without a terminal wrote ~/.ssh/config")
	}
	if out, _, code := run("revoke", f.computer.ID); code != 0 || !strings.Contains(out, "这台电脑的钥匙已撤销") {
		t.Fatalf("revoke: %d %q", code, out)
	}
	if _, stderr, code := run("revoke", "nope"); code != 1 || !strings.HasPrefix(stderr, "Super Cage：") {
		t.Fatalf("revoke unknown: %d %q", code, stderr)
	}
	for _, args := range [][]string{{}, {"bogus"}, {"revoke"}, {"rename", "x"}, {"status", "a", "b"}} {
		if _, stderr, code := run(args...); code != 2 || !strings.HasPrefix(stderr, "用法：") {
			t.Fatalf("%q: %d %q", args, code, stderr)
		}
	}
	if _, stderr, code := run("status", "../x"); code == 0 || !strings.HasPrefix(stderr, "Super Cage：") {
		t.Fatalf("bad environment: %d %q", code, stderr)
	}
	if out, _, code := run("off"); code != 0 || !strings.Contains(out, "未开启") {
		t.Fatalf("off: %d %q", code, out)
	}
	if _, err := os.Stat(f.paths.clientDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("off left Super Cage's ssh files")
	}
}

// cage entry on asks the person (owner decision 2026-11-22): with stdin or
// stderr not a terminal it refuses before it asks, even when the other is
// one; it never opens the controlling terminal then.
func TestEntryOnRefusesWithoutBothTerminals(t *testing.T) {
	f := newEntryFixture(t, true)
	_, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var buffer bytes.Buffer
	for name, c := range map[string]struct {
		stdin  *os.File
		stderr io.Writer
	}{"stdin a pty, stderr a buffer": {tty, &buffer}, "stdin /dev/null, stderr a pty": {devnull, tty}, "no stdin": {nil, tty}} {
		var stdout bytes.Buffer
		if code := EntryMain([]string{"on"}, f.s.configPath, c.stdin, &stdout, c.stderr); code != 1 || stdout.Len() != 0 {
			t.Errorf("%s: %d %q", name, code, stdout.String())
		}
	}
	if !strings.Contains(buffer.String(), entryOnRefusal) {
		t.Errorf("the refusal: %q", buffer.String())
	}
	var view entryStatusView
	if err := f.call("GET", "/api/entry", nil, &view); err != nil || view.Enabled {
		t.Fatalf("the entry turned on: %+v %v", view, err)
	}
}

// With a person at a terminal, cage entry on asks on the controlling
// terminal: N (or nothing) leaves the entry off, y turns it on and goes on to
// the Include question. The helper process runs cage entry on with a pty as
// its controlling terminal, stdin, stdout and stderr.
func TestEntryOnAsksThePerson(t *testing.T) {
	f := newEntryFixture(t, true)
	run := func(answers ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestEntryOnHelper$", "-test.count=1")
		cmd.Env = f.sshEnv("CAGE_ENTRY_ON_HELPER=1", "TERM=dumb")
		master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 200, Rows: 30})
		if err != nil {
			t.Fatal(err)
		}
		defer master.Close()
		output := &entrySyncBuffer{}
		go func() { _, _ = io.Copy(output, master) }()
		for i, answer := range answers {
			entryAwait(t, 20*time.Second, "question "+strconv.Itoa(i+1), func() bool { return strings.Count(output.String(), "[y/N] ") > i })
			if _, err := io.WriteString(master, answer+"\n"); err != nil {
				t.Fatal(err)
			}
		}
		err = cmd.Wait()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		return output.String(), code
	}
	enabled := func() bool {
		t.Helper()
		var view entryStatusView
		if err := f.call("GET", "/api/entry", nil, &view); err != nil {
			t.Fatal(err)
		}
		return view.Enabled
	}
	for _, answer := range []string{"n", "", "yy"} {
		out, code := run(answer)
		if code != 1 || !strings.Contains(out, "在这台电脑上开启默认环境的本机终端？") || !strings.Contains(out, "未开启。") || enabled() {
			t.Fatalf("answer %q: %d %q", answer, code, out)
		}
	}
	// The answer comes from the controlling terminal, not from stdin: a "y"
	// on stdin (another terminal here) does not turn the entry on.
	controlling, controllingTTY, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer controlling.Close()
	input, inputTTY, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEntryOnHelper$", "-test.count=1")
	cmd.Env = f.sshEnv("CAGE_ENTRY_ON_HELPER=1", "TERM=dumb")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputTTY, controllingTTY, controllingTTY
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 2}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = controllingTTY.Close()
	_ = inputTTY.Close()
	output := &entrySyncBuffer{}
	go func() { _, _ = io.Copy(output, controlling) }()
	go func() { _, _ = io.Copy(io.Discard, input) }()
	if _, err := io.WriteString(input, "y\n"); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 20*time.Second, "the question on the controlling terminal", func() bool { return strings.Contains(output.String(), "[y/N] ") })
	if _, err := io.WriteString(controlling, "n\n"); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err == nil || err.(*exec.ExitError).ExitCode() != 1 || enabled() {
			t.Fatalf("a y on stdin: %v %q", err, output.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("cage entry on did not take the controlling terminal's answer: %q (enabled %v)", output.String(), enabled())
	}
	out, code := run("y", "n")
	if code != 0 || !strings.Contains(out, "已开启 · 没有连接") || !strings.Contains(out, "写入吗？[y/N] ") || !strings.Contains(out, "不加这一行也能用：cage ssh default") || !enabled() {
		t.Fatalf("answer y: %d %q", code, out)
	}
	if _, err := os.Stat(f.paths.userConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("answering n to the Include wrote ~/.ssh/config")
	}
}

// TestEntryOnHelper is cage entry on in a helper process of
// TestEntryOnAsksThePerson.
func TestEntryOnHelper(t *testing.T) {
	if os.Getenv("CAGE_ENTRY_ON_HELPER") == "" {
		t.Skip("helper process only")
	}
	os.Exit(EntryMain([]string{"on"}, os.Getenv("CAGE_ENTRY_TEST_CONFIG"), os.Stdin, os.Stdout, os.Stderr))
}
