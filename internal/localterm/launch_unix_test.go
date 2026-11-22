//go:build !windows

package localterm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A launch writes only into a private directory of its own, and only files
// that did not exist: never through a link, never over another file. A
// launch that fails leaves nothing behind.
func TestLaunchFilesAreExclusive(t *testing.T) {
	plan, script := planFor(t, "kitty")
	scratch := filepath.Dir(script)
	// The directory was left open to others: it is made private.
	if err := os.Chmod(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Launch(context.Background(), plan, &recordingRunner{}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(scratch); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("scratch directory not private", info.Mode(), err)
	}
	// The file is already there: refused, and left as it was.
	if err := Launch(context.Background(), plan, &recordingRunner{}); err == nil {
		t.Fatal("a launch wrote over an existing file")
	}
	_ = os.Remove(script)
	// A link at the file's path: refused, its target untouched.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, script); err != nil {
		t.Fatal(err)
	}
	if err := Launch(context.Background(), plan, &recordingRunner{}); err == nil {
		t.Fatal("a launch wrote through a link")
	}
	if data, _ := os.ReadFile(victim); string(data) != "mine" {
		t.Fatalf("the link's target was written: %q", data)
	}
	_ = os.Remove(script)
	// The directory is a link: refused.
	moved := scratch + "-real"
	if err := os.Rename(scratch, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, scratch); err != nil {
		t.Fatal(err)
	}
	if err := Launch(context.Background(), plan, &recordingRunner{}); err == nil {
		t.Fatal("a launch used a linked directory")
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(script))); !os.IsNotExist(err) {
		t.Fatal("a file was written through the linked directory")
	}
	_ = os.Remove(scratch)
	_ = os.Rename(moved, scratch)
	// Every step failed: nothing is left behind.
	if err := Launch(context.Background(), plan, &recordingRunner{fail: 9}); err == nil {
		t.Fatal("all steps failed but launch succeeded")
	}
	if _, err := os.Lstat(script); !os.IsNotExist(err) {
		t.Fatal("a failed launch left its script", err)
	}
}

// A launch's files go after launchFileLife; a file put at the same path
// since stays.
func TestLaunchFilesExpire(t *testing.T) {
	life := launchFileLife
	launchFileLife = 50 * time.Millisecond
	t.Cleanup(func() { launchFileLife = life })
	plan, script := planFor(t, "kitty")
	if err := Launch(context.Background(), plan, &recordingRunner{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(script); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the script never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	launchFileLife = time.Hour
	if err := Launch(context.Background(), plan, &recordingRunner{}); err != nil {
		t.Fatal(err)
	}
	launchFileLife = 50 * time.Millisecond
	_ = os.Remove(script)
	if err := os.WriteFile(script, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Lstat(script); err != nil {
		t.Fatal("a file that replaced the script was removed")
	}
}

// An AppleScript step may wait for the owner far longer than other steps.
func TestPromptStepsWaitForTheOwner(t *testing.T) {
	step, prompt := stepWait, promptWait
	stepWait, promptWait = 100*time.Millisecond, 3*time.Second
	t.Cleanup(func() { stepWait, promptWait = step, prompt })
	slow := []string{"/bin/sleep", "0.5"}
	if err := (ExecRunner{}).Run(context.Background(), Step{Argv: slow}); err == nil {
		t.Fatal("a plain step outlived its bound")
	}
	if err := (ExecRunner{}).Run(context.Background(), Step{Argv: slow, Prompt: true}); err != nil {
		t.Fatal("an AppleScript step was cut short:", err)
	}
}

// kitty and WezTerm are started directly and may become the owner's running
// application: they get the owner's session environment (an SSH agent,
// what the owner set with launchctl setenv), as from the Dock, and nothing
// of Super Cage's own.
func TestDirectLaunchGetsTheSessionEnvironment(t *testing.T) {
	wait := detachWait
	detachWait = 300 * time.Millisecond
	environ := launchdEnviron
	launchdEnviron = func() []string { return []string{"SSH_AUTH_SOCK=/private/tmp/agent.sock", "OWNER_SETTING=1"} }
	t.Cleanup(func() { detachWait, launchdEnviron = wait, environ })
	t.Setenv("CAGE_OWN_SETTING", "cage")
	out := filepath.Join(t.TempDir(), "env")
	step := Step{Argv: []string{"/bin/sh", "-c", "env > " + out + "; sleep 1"}, Detach: true}
	if err := (ExecRunner{}).Run(context.Background(), step); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	for _, want := range []string{"SSH_AUTH_SOCK=/private/tmp/agent.sock\n", "OWNER_SETTING=1\n", "HOME="} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in %q", want, env)
		}
	}
	if strings.Contains(env, "CAGE_OWN_SETTING") {
		t.Error("Super Cage's own environment reached the application")
	}
}

func TestLaunchdEnvironmentNames(t *testing.T) {
	sample := "gui/501 = {\n\ttype = gui\n\tenvironment = {\n\t\tSSH_AUTH_SOCK => /private/tmp/x/Listeners\n\t\tMY_VAR => a => b\n\t}\n\tservices = {\n\t\tFOO => 1\n\t}\n}\n"
	if got := strings.Join(launchdNames(sample), ","); got != "SSH_AUTH_SOCK,MY_VAR" {
		t.Fatal(got)
	}
}
