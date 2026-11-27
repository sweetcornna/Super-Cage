package cage

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestDshSSHRecoveryPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dsh SSH recovery uses Linux terminal and file APIs")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	if output, err := exec.Command(python, "-IB", "dsh-ssh.test.py").CombinedOutput(); err != nil {
		t.Fatalf("dsh SSH tests: %v\n%s", err, output)
	}
}

func TestDshSSHGuestProvisioning(t *testing.T) {
	script := componentStartScript()
	if got := strings.Count(script, dshSSHScript()); got != 1 {
		t.Fatalf("dsh SSH installation appears %d times in guest component script", got)
	}
	if strings.Contains(dshSSHScript(), "apt-get") {
		t.Fatal("dsh SSH recovery should reuse the guest's installed tmux")
	}
}
