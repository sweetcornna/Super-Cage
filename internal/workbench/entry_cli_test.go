//go:build !windows

package workbench

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, stderr, code := run("herdr"); code != 1 || !strings.Contains(stderr, "cage entry on default") {
		t.Fatalf("herdr while off: %d %q", code, stderr)
	}
	if _, stderr, code := run("off"); code != 1 || stderr != "Super Cage："+errEntryOff.Message+"\n" {
		t.Fatalf("off while off: %d %q", code, stderr)
	}
	out, stderr, code := run("on")
	if code != 0 || !strings.Contains(out, "已开启 · 没有连接") || !strings.Contains(out, "任何终端：ssh cage-default") || !strings.Contains(out, "不加这一行也能用：cage ssh default") {
		t.Fatalf("on: %d %q %q", code, out, stderr)
	}
	if _, err := os.Stat(f.paths.userConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cage entry on without a terminal wrote ~/.ssh/config")
	}
	f.loadDevice()
	// cage entry herdr prints the command and never runs an hc.
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte("#!/bin/sh\n: >"+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _, code = run("herdr")
	if code != 0 || !strings.Contains(out, "在这台电脑的终端里运行：\n\n  hc machine add ") || !strings.HasSuffix(out, entryHerdrNote+"\n") {
		t.Fatalf("herdr: %d %q", code, out)
	}
	var herdr entryHerdrView
	if out, _, code := run("herdr", "--json"); code != 0 || json.Unmarshal([]byte(out), &herdr) != nil || herdr.State != "not_set_up" || !strings.HasPrefix(herdr.Command, "hc machine add ") {
		t.Fatalf("herdr --json: %d %q", code, out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("cage entry herdr ran hc")
	}
	out, _, code = run("status", "--json")
	var view entryStatusView
	if code != 0 || json.Unmarshal([]byte(out), &view) != nil || !view.Enabled || view.HostAlias != "cage-default" || view.Commands == nil {
		t.Fatalf("status --json: %d %q", code, out)
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
