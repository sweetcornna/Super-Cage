package workbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
)

// startFailureOutput is what cage up prints when Lima.Start fails: cli
// boot()'s "Error: Failed to start VM" block, with the cause on a line of its
// own after Lima's output and no "Error: " prefix.
func startFailureOutput(cause string) string {
	return "Starting VM 'cage'...\nError: Failed to start VM 'cage'.\n--- Lima's output ---\n" +
		"time=\"2026-11-23T01:00:00Z\" level=info msg=\"Starting the instance \\\"cage\\\" with internal VM driver \\\"qemu\\\"\"\n" +
		cause + "\nFull log: /home/cage/.claude-cage/state/lima/cage/ha.stderr.log\n"
}

func TestRunCageExplainsAStartRefusedForMemory(t *testing.T) {
	if output := os.Getenv("CAGE_TEST_START_MEMORY_FAILURE"); output != "" {
		fmt.Fprint(os.Stderr, output)
		os.Exit(1)
	}
	for _, tc := range []struct{ name, output, want string }{
		// The incident (0.1.16): QEMU's reason, as vmStartError adds it.
		{"qemu", startFailureOutput("QEMU 启动失败：qemu-system-x86_64: cannot set up guest memory 'pc.ram': Cannot allocate memory；host agent process has exited"), qemuMemorySentence},
		{"qemu after this change", startFailureOutput("QEMU 启动失败：内存不足，这台机器现在映射不了环境的内存上限（qemu-system-x86_64: cannot set up guest memory 'pc.ram': Cannot allocate memory）；exiting"), qemuMemorySentence},
		// The start refused before QEMU (cage.PrepareBoot).
		{"prepare", startFailureOutput((&cage.BootMemoryError{Need: 2304 << 20, Have: 1738 << 20, Growable: true}).Error()), "内存不足以启动环境：工作区至少需要约 2.2 GB，现有 1.7 GB"},
		// Any other QEMU reason in that format now reaches job.error too.
		{"other qemu", startFailureOutput("QEMU 启动失败：WHPX: Failed to enable nested virtualization, hr=80370302；host agent process has exited"), "QEMU 启动失败：WHPX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAGE_TEST_START_MEMORY_FAILURE", tc.output)
			s := testServer(t)
			if err := writePrivate(s.profile+string(os.PathSeparator)+"environment.json", map[string]string{"directory": t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			s.exe = exe
			if _, err := s.submitJob("start", func() error { return s.runCage("-test.run=^TestRunCageExplainsAStartRefusedForMemory$") }); err != nil {
				t.Fatal(err)
			}
			job := awaitJobIdle(t, &s.jobs)
			if !strings.Contains(job.Error, tc.want) || strings.Contains(job.Error, "退出码") || job.Message != "启动环境失败" {
				t.Fatalf("job %+v", job)
			}
		})
	}
}

func TestJobMessageLineSetsTheMessage(t *testing.T) {
	t.Setenv(cage.JobEnv, "job-1")
	s := testServer(t)
	shown, release := make(chan struct{}), make(chan struct{})
	if _, err := s.submitJob("start", func() error {
		execution := s.jobs.execution()
		fmt.Fprintln(execution, "Starting VM 'cage'...")
		cage.JobMessage(execution, "正在向服务器申请内存…")
		close(shown)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-shown
	var job Job
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if job = s.jobs.snapshot(); job.Message == "正在向服务器申请内存…" {
			break
		}
	}
	close(release)
	if job.Message != "正在向服务器申请内存…" || strings.Contains(job.Log, cage.JobMessagePrefix) || !strings.Contains(job.Log, "Starting VM") {
		t.Fatalf("job %+v", job)
	}
	awaitJobIdle(t, &s.jobs)
}

func TestResourceStatusSaysTheStartFellBack(t *testing.T) {
	oldMem := host.MemGiB
	t.Cleanup(func() { host.MemGiB = oldMem })
	host.MemGiB = func() int { return 9 }
	t.Setenv("CAGE_MACHINE_ROLE", "dedicated")
	s := testServer(t)
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if !s.vmRunning() {
		t.Fatal("the running-environment fixture is not seen")
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "lima.yaml"), []byte("memory: 4GiB\ncpus: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notice := "服务器暂时分不出更多内存，环境按 4 GB 内存上限启动（设置为 7 GB），之后重启环境即可恢复。"
	read := func() (status struct {
		Memory  int    `json:"memory_gib"`
		Restart bool   `json:"restart_required"`
		Notice  string `json:"notice"`
	}) {
		t.Helper()
		w := request(s, "GET", "/api/environment/resources", "", "", true)
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		return status
	}
	if got := read(); got.Memory != 7 || !got.Restart || got.Notice != "" {
		t.Fatalf("without a fallback %+v", got)
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), cage.BootNoticeFile), []byte(notice), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Notice != notice || !got.Restart {
		t.Fatalf("after a fallback %+v", got)
	}
}
