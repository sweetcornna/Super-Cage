package workbench

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

func TestSuperCagePhoneInBrowser(t *testing.T) {
	if os.Getenv("CAGE_TEST_MOBILE_BROWSER") == "" {
		t.Skip("set CAGE_TEST_MOBILE_BROWSER after building the phone UI")
	}
	s := testServer(t)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), map[string]string{"directory": project}); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberSharedProject(project); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"README.md": "# Super Cage 测试项目\n\n用于手机端测试。\n", "src/app.py": "print('hi')\n"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.NewString()
	if _, err := s.mobileCLI.create(s.profile, project, id); err != nil {
		t.Fatal(err)
	}
	rec, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	rec.Title = "整理发布说明"
	rec.Turns = []mobileTurn{{ID: uuid.NewString(), Prompt: "整理 0.1.15 的发布说明", Status: "completed", Text: "发布说明已整理好，放在 `docs/release.md`。"}}
	if err = writePrivate(mobileRecordPath(s.profile, id), rec); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.handler())
	defer host.Close()
	s.origin = host.URL
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	backend, err := mobilelink.OpenWorkbench(ctx, host.URL, s.issueTicket())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	admin := strings.Repeat("q", 48)
	service, err := relay.New(relay.Config{AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	relayHTTP := httptest.NewServer(service)
	defer relayHTTP.Close()
	room, err := mobilelink.CreateRoom(ctx, relayHTTP.URL, admin)
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	phoneID, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	go mobilelink.ServeHost(ctx, relayHTTP.URL, room.ID, room.HostToken, hostID, phoneID.Pin, mobilelink.Opened(backend))
	phone, err := mobilelink.ConnectClient(ctx, relayHTTP.URL, room.ID, room.ClientToken, phoneID, hostID.Pin)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	web := httptest.NewUnstartedServer(nil)
	defer web.Close()
	assets, err := filepath.Abs("../../mobile/.build/resources/mobile-ui")
	if err != nil {
		t.Fatal(err)
	}
	handler, entry, err := mobilelink.NewBrowser("http://"+web.Listener.Addr().String(), assets, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	web.Config.Handler = handler
	web.Start()
	driver := exec.CommandContext(ctx, "node", "../../ui/scripts/super-cage-phone-smoke.mjs")
	driver.Stdin = strings.NewReader(entry)
	if output, err := driver.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	if data, err := os.ReadFile(filepath.Join(project, "photo.txt")); err != nil || string(data) != "uploaded from phone\n" {
		t.Fatalf("uploaded file: %q %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(project, "notes")); err != nil || !info.IsDir() {
		t.Fatalf("created folder: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(project, ".super-cage", "attachments", "*-screen.png"))
	if len(matches) != 1 {
		t.Fatalf("chat attachment: %v", matches)
	}
}
