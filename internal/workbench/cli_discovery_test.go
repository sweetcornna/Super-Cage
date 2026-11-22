//go:build !windows

package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
)

func TestCLIDiscoveryHelper(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	if output, err := exec.Command(python, "-I", "cli_discovery_test.py").CombinedOutput(); err != nil {
		t.Fatalf("resident discovery helper: %v\n%s", err, output)
	}
}

func TestCLIDiscoveryCommandSize(t *testing.T) {
	// One argument of a Linux command line holds at most 128 KiB.
	t.Logf("resident helper command: %d bytes", len(guestLoginCommand(cliDiscoveryCommand())))
	if size := len(guestLoginCommand(cliDiscoveryCommand())); size > 32<<10 {
		t.Fatalf("resident helper command uses %d bytes", size)
	}
}

// observedGuest keeps the real host bootstrap and SSH path and replaces only
// the guest helper with one that prints the observation file.
func observedGuest(t *testing.T, s *Server, home string) func([]terminalConversation) {
	t.Helper()
	return observedGuestWith(t, home, "")
}

// guestHelper replaces the versioned channel helper in the guest home.
func guestHelper(t *testing.T, home, code string) {
	t.Helper()
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
}

// observedGuestWith runs prelude (Python) in the guest helper before it
// prints the observation.
func observedGuestWith(t *testing.T, home, prelude string) func([]terminalConversation) {
	t.Helper()
	observation := filepath.Join(home, "observation.json")
	guestHelper(t, home, fmt.Sprintf("import json,sys,time\njson.load(sys.stdin)\n%s\nprint(open(%q).read())\n", prelude, observation))
	return func(found []terminalConversation) {
		t.Helper()
		if err := writePrivate(observation, found); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoveryRewritesOnlyChangedRecords(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false); err != nil {
		t.Fatal(err)
	}
	observe := observedGuest(t, s, home)
	s.ready = true
	item := terminalConversation{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "first", UpdatedAt: 1700000000}
	observe([]terminalConversation{item})
	path := mobileRecordPath(s.profile, id)
	discover := func() os.FileInfo {
		t.Helper()
		if err := s.discoverTerminalConversations(context.Background(), []string{project}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	first := discover()
	if second := discover(); !os.SameFile(first, second) {
		t.Fatal("an unchanged observation rewrote the session record")
	}
	item.Title = "second"
	observe([]terminalConversation{item})
	if third := discover(); os.SameFile(first, third) {
		t.Fatal("a changed observation was not written")
	}
	if saved, err := s.mobileCLI.record(s.profile, id); err != nil || saved.Title != "second" {
		t.Fatalf("record = %+v, %v", saved, err)
	}
}

func sessionList(t *testing.T, s *Server, query string) (int, sessionListAnswer) {
	t.Helper()
	return listAt(t, s, "/api/cli/sessions"+query)
}

func phoneList(t *testing.T, s *Server, query string) (int, sessionListAnswer) {
	t.Helper()
	return listAt(t, s, "/api/mobile/cli/sessions"+query)
}

func listAt(t *testing.T, s *Server, path string) (int, sessionListAnswer) {
	t.Helper()
	w := request(s, "GET", path, s.origin, "", true)
	var answer sessionListAnswer
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, answer
}

func TestSessionListRevisionAnswersNoContentWhenUnchanged(t *testing.T) {
	s, id, project := metadataFixture(t)
	code, first := sessionList(t, s, "")
	if code != http.StatusOK || len(first.Sessions) != 1 || len(first.Revision) != 32 {
		t.Fatalf("list = %d %+v", code, first)
	}
	if code, _ := sessionList(t, s, "?after="+first.Revision); code != http.StatusNoContent {
		t.Fatalf("unchanged list answered %d", code)
	}
	if code, _ := sessionList(t, s, "?archived=true&after="+first.Revision); code != http.StatusOK {
		t.Fatalf("the archived list shares a revision with the active list: %d", code)
	}
	if w := request(s, "POST", "/api/cli/sessions/"+id+"/metadata", s.origin, `{"title":"改名"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	code, renamed := sessionList(t, s, "?after="+first.Revision)
	if code != http.StatusOK || renamed.Revision == first.Revision || renamed.Sessions[0].Title != "改名" {
		t.Fatalf("a rename did not reach the conditional answer: %d %+v", code, renamed)
	}
	created := uuid.NewString()
	if _, err := s.mobileCLI.create(s.profile, project, created); err != nil {
		t.Fatal(err)
	}
	code, grown := sessionList(t, s, "?after="+renamed.Revision)
	if code != http.StatusOK || len(grown.Sessions) != 2 {
		t.Fatalf("a created session did not reach the conditional answer: %d %+v", code, grown)
	}
	if w := request(s, "POST", "/api/cli/sessions/"+id+"/archive", s.origin, "{}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	code, shrunk := sessionList(t, s, "?after="+grown.Revision)
	if code != http.StatusOK || len(shrunk.Sessions) != 1 || shrunk.Sessions[0].ID != created {
		t.Fatalf("an archived session stayed in the conditional answer: %d %+v", code, shrunk)
	}
	if _, archived := sessionList(t, s, "?archived=true"); len(archived.Sessions) != 1 || archived.Sessions[0].ID != id {
		t.Fatalf("archived list = %+v", archived)
	}
	for _, query := range []string{"?after=xyz", "?after=" + strings.ToUpper(first.Revision), "?wait=5", "?after=" + first.Revision + "&wait=-1"} {
		if code, _ := sessionList(t, s, query); code != http.StatusBadRequest {
			t.Fatalf("%s answered %d", query, code)
		}
	}
}

func TestConcurrentSessionListsShareDiscovery(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(home, "runs")
	observe := observedGuestWith(t, home, fmt.Sprintf("open(%q,'a').write('run\\n')\ntime.sleep(0.4)", runs))
	observe([]terminalConversation{{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "guest", UpdatedAt: 1700000000}})
	s.ready = true
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = request(s, "GET", "/api/cli/sessions", s.origin, "", true).Code
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(runs)
	if count := strings.Count(string(data), "run"); count < 1 || count > 2 {
		t.Fatalf("six concurrent lists ran %d discoveries", count)
	}
	for _, code := range codes {
		if code != http.StatusOK {
			t.Fatal(codes)
		}
	}
}

func TestSessionListNeverSharesAnEarlierDiscovery(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	// The guest reads its observation, marks that it did, and answers later.
	marker := filepath.Join(home, "observed")
	observation := filepath.Join(home, "observation.json")
	guestHelper(t, home, fmt.Sprintf("import json,sys,time\njson.load(sys.stdin)\ndata=open(%q).read()\nopen(%q,'a').write('x')\ntime.sleep(0.6)\nprint(data)\n", observation, marker))
	item := terminalConversation{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "before", UpdatedAt: 1700000000}
	if err := writePrivate(observation, []terminalConversation{item}); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	earlier := make(chan sessionListAnswer, 1)
	go func() {
		_, answer := sessionList(t, s, "")
		earlier <- answer
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first discovery did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	item.Title = "after"
	if err := writePrivate(observation, []terminalConversation{item}); err != nil {
		t.Fatal(err)
	}
	// This request arrives while a run that read the old title is in flight;
	// it must answer with an observation made after its own arrival.
	_, latest := sessionList(t, s, "")
	if first := <-earlier; len(first.Sessions) != 1 || first.Sessions[0].Title != "before" {
		t.Fatalf("first list = %+v", first.Sessions)
	}
	if len(latest.Sessions) != 1 || latest.Sessions[0].Title != "after" {
		t.Fatalf("later list = %+v", latest.Sessions)
	}
}

// residentGuest prepares a guest home with a login profile and a claude CLI
// that count their runs, and the real channel helper.
func residentGuest(t *testing.T, s *Server) (home string, logins, claudeRuns func() int) {
	t.Helper()
	home, _ = filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	cli := "#!/bin/sh\n[ \"$1\" = agents ] || exit 64\necho run >> \"$HOME/claude-runs\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	profile := "echo login >> \"$HOME/logins\"\nexport PATH=" + shellescape.Quote(bin) + ":/usr/bin:/bin\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude")})
	t.Cleanup(s.cliDiscovery.resident.close)
	count := func(name string) func() int {
		return func() int {
			data, _ := os.ReadFile(filepath.Join(home, name))
			return strings.Count(string(data), "\n")
		}
	}
	return home, count("logins"), count("claude-runs")
}

func writeTranscript(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	stream, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, row := range rows {
		data, _ := json.Marshal(row)
		if _, err := stream.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionListsUseOneResidentGuestHelper(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, claudeRuns := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	title := func() string {
		t.Helper()
		code, answer := sessionList(t, s, "")
		if code != http.StatusOK || len(answer.Sessions) != 1 || answer.Notice != "" {
			t.Fatalf("list = %d %+v", code, answer)
		}
		return answer.Sessions[0].Title
	}
	// The first request installs the channel helper with an ordinary login;
	// the second starts the resident helper with one more.
	for want := 1; want <= 2; want++ {
		if got := title(); got != "guest title" || logins() != want || claudeRuns() != want {
			t.Fatalf("request %d: title %q, %d logins, %d CLI listings", want, got, logins(), claudeRuns())
		}
	}
	for range 3 {
		if got := title(); got != "guest title" || logins() != 2 || claudeRuns() != 2 {
			t.Fatalf("an unchanged list cost a login or a CLI listing: title %q, %d logins, %d listings", got, logins(), claudeRuns())
		}
	}
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "renamed in the guest"})
	if got := title(); got != "renamed in the guest" || logins() != 2 {
		t.Fatalf("a changed transcript: title %q after %d logins", got, logins())
	}
	// Reading the session itself uses the same helper.
	for range 2 {
		if w := request(s, "GET", "/api/mobile/cli/sessions/"+id, s.origin, "", true); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if logins() != 2 {
		t.Fatalf("session reads opened %d logins", logins()-2)
	}
	if s.cliDiscovery.viewKeys[id].key == "" {
		t.Fatal("the resident view left no watch key")
	}
	// A resident helper that ends while a list is held is replaced with one
	// login, without backing off to a login per request.
	_, current := sessionList(t, s, "")
	held := make(chan sessionListAnswer, 1)
	go func() {
		_, answer := sessionList(t, s, "?after="+current.Revision+"&wait=20")
		held <- answer
	}()
	time.Sleep(500 * time.Millisecond)
	// Ending the connection looks to the workbench as the helper or the guest
	// going away: the held watch fails with errGuestSessionEnded.
	s.cliDiscovery.resident.close()
	time.Sleep(500 * time.Millisecond)
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "after the helper ended"})
	select {
	case answer := <-held:
		if len(answer.Sessions) != 1 || answer.Sessions[0].Title != "after the helper ended" {
			t.Fatalf("held list = %+v", answer)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a held list missed a change after the helper ended")
	}
	if got := title(); got != "after the helper ended" || logins() != 3 {
		t.Fatalf("after the helper ended: title %q, %d logins", got, logins())
	}
}

func TestResidentHelperRetiresWithoutBackingOff(t *testing.T) {
	previous := residentLifetime
	residentLifetime = 2 * time.Second
	t.Cleanup(func() { residentLifetime = previous })
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	if logins() != 2 {
		t.Fatalf("%d logins before retiring", logins())
	}
	time.Sleep(2200 * time.Millisecond)
	for range 2 {
		if _, answer := sessionList(t, s, ""); len(answer.Sessions) != 1 || answer.Sessions[0].Title != "guest title" {
			t.Fatalf("list = %+v", answer)
		}
	}
	if logins() != 3 {
		t.Fatalf("a retired helper cost %d logins instead of one", logins()-2)
	}
}

func TestHeldSessionListWithoutResidentHelper(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	t.Cleanup(s.cliDiscovery.resident.close)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	// The substituted helper does not match its digest, so no resident
	// helper starts and the held list looks at the guest itself.
	observe := observedGuest(t, s, home)
	item := terminalConversation{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "before", UpdatedAt: 1700000000}
	observe([]terminalConversation{item})
	s.ready = true
	sessionList(t, s, "")
	_, current := sessionList(t, s, "")
	held := make(chan sessionListAnswer, 1)
	go func() {
		_, answer := sessionList(t, s, "?after="+current.Revision+"&wait=20")
		held <- answer
	}()
	time.Sleep(300 * time.Millisecond)
	item.Title = "after"
	observe([]terminalConversation{item})
	changed := time.Now()
	select {
	case answer := <-held:
		if len(answer.Sessions) != 1 || answer.Sessions[0].Title != "after" || time.Since(changed) > 8*time.Second {
			t.Fatalf("held list = %+v after %v", answer, time.Since(changed))
		}
		t.Logf("without the resident helper the change arrived after %v", time.Since(changed))
	case <-time.After(15 * time.Second):
		t.Fatal("a held list without the resident helper missed a guest change")
	}
}

func TestSessionReadKeepsTheDiscoveredTitle(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	command := "<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args>opus</command-args>"
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"),
		map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": command}},
		map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "继续"}})
	s.ready = true
	_, listed := sessionList(t, s, "")
	if len(listed.Sessions) != 1 || listed.Sessions[0].Title != "/model" {
		t.Fatalf("list = %+v", listed)
	}
	read := func() {
		t.Helper()
		w := request(s, "GET", "/api/mobile/cli/sessions/"+id, s.origin, "", true)
		var view mobileSession
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Title != "/model" {
			t.Fatalf("read = %d %s", w.Code, w.Body.String())
		}
	}
	// The first read loads the history, a new session revision.
	read()
	_, listed = sessionList(t, s, "")
	read()
	if code, answer := sessionList(t, s, "?after="+listed.Revision); code != http.StatusNoContent {
		t.Fatalf("reading an unchanged session changed the list: %d %+v", code, answer.Sessions)
	}
}

func TestHeldSessionListAnswersHostChanges(t *testing.T) {
	s, id, project := metadataFixture(t)
	_, first := sessionList(t, s, "")
	started := time.Now()
	if code, _ := sessionList(t, s, "?after="+first.Revision+"&wait=1"); code != http.StatusNoContent || time.Since(started) < time.Second {
		t.Fatalf("an unchanged held list answered %d after %v", code, time.Since(started))
	}
	held := make(chan sessionListAnswer, 1)
	go func() {
		_, answer := sessionList(t, s, "?after="+first.Revision+"&wait=20")
		held <- answer
	}()
	time.Sleep(300 * time.Millisecond)
	renamed := time.Now()
	if w := request(s, "POST", "/api/cli/sessions/"+id+"/metadata", s.origin, `{"title":"保持等待时改名"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case answer := <-held:
		if len(answer.Sessions) != 1 || answer.Sessions[0].Title != "保持等待时改名" || time.Since(renamed) > 3*time.Second {
			t.Fatalf("held list = %+v after %v", answer, time.Since(renamed))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a held list missed a rename")
	}
	_, current := sessionList(t, s, "")
	go func() {
		_, answer := sessionList(t, s, "?after="+current.Revision+"&wait=20")
		held <- answer
	}()
	time.Sleep(300 * time.Millisecond)
	created := time.Now()
	if _, err := s.mobileCLI.create(s.profile, project, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-held:
		if len(answer.Sessions) != 2 || time.Since(created) > 3*time.Second {
			t.Fatalf("held list = %+v after %v", answer, time.Since(created))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a held list missed a new session")
	}
	s.cliDiscovery.mu.Lock()
	watchers := s.cliDiscovery.watchers
	s.cliDiscovery.mu.Unlock()
	if watchers != 0 {
		t.Fatalf("%d watchers remain", watchers)
	}
}

func TestHeldSessionListWakesOnGuestChange(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	_, current := sessionList(t, s, "")
	held := make(chan sessionListAnswer, 1)
	go func() {
		code, answer := sessionList(t, s, "?after="+current.Revision+"&wait=20")
		if code != http.StatusOK {
			t.Errorf("held list answered %d", code)
		}
		held <- answer
	}()
	time.Sleep(500 * time.Millisecond)
	changed := time.Now()
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "named while held"})
	select {
	case answer := <-held:
		if len(answer.Sessions) != 1 || answer.Sessions[0].Title != "named while held" {
			t.Fatalf("held list = %+v", answer)
		}
		if elapsed := time.Since(changed); elapsed > 4*time.Second {
			t.Fatalf("the guest change took %v to arrive", elapsed)
		}
		t.Logf("guest change reached the held list after %v", time.Since(changed))
	case <-time.After(15 * time.Second):
		t.Fatal("a held list missed a guest change")
	}
	if logins() != 2 {
		t.Fatalf("waiting opened %d more logins", logins()-2)
	}
}

func TestHeldSessionViewAnswersGuestReply(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "question"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	read := func(query string) (int, mobileSession) {
		w := request(s, "GET", "/api/mobile/cli/sessions/"+id+query, s.origin, "", true)
		var view mobileSession
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, view
	}
	_, view := read("")
	query := fmt.Sprintf("?after=%d&epoch=%s", view.Revision, view.Generation)
	started := time.Now()
	if code, _ := read(query + "&wait=1"); code != http.StatusNoContent || time.Since(started) < time.Second {
		t.Fatalf("an unchanged held view answered %d after %v", code, time.Since(started))
	}
	held := make(chan mobileSession, 1)
	go func() {
		code, view := read(query + "&wait=20")
		if code != http.StatusOK {
			t.Errorf("held view answered %d", code)
		}
		held <- view
	}()
	time.Sleep(500 * time.Millisecond)
	replied := time.Now()
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "assistant", "uuid": uuid.NewString(), "message": map[string]any{"content": []map[string]string{{"type": "text", "text": "answer"}}}})
	select {
	case view := <-held:
		if len(view.Turns) != 1 || view.Turns[0].Text != "answer" {
			t.Fatalf("held view = %+v", view.Turns)
		}
		if elapsed := time.Since(replied); elapsed > 2*time.Second {
			t.Fatalf("the reply took %v to arrive", elapsed)
		}
		t.Logf("guest reply reached the held view after %v", time.Since(replied))
	case <-time.After(15 * time.Second):
		t.Fatal("a held view missed the reply")
	}
	if logins() != 2 {
		t.Fatalf("waiting opened %d more logins", logins()-2)
	}
	if code, _ := read(query + "&wait=x"); code != http.StatusBadRequest {
		t.Fatalf("a malformed wait answered %d", code)
	}
}

func TestHeldRequestsEndWhenTheServerCloses(t *testing.T) {
	s, _, project := metadataFixture(t)
	hosted := uuid.NewString()
	if _, err := s.mobileCLI.create(s.profile, project, hosted); err != nil {
		t.Fatal(err)
	}
	_, list := sessionList(t, s, "")
	w := request(s, "GET", "/api/mobile/cli/sessions/"+hosted, s.origin, "", true)
	var view mobileSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	// The request context ends with the server; the middleware then aborts
	// the response, so only the prompt end is asserted.
	ended := make(chan struct{}, 2)
	go func() {
		request(s, "GET", "/api/cli/sessions?after="+list.Revision+"&wait=20", s.origin, "", true)
		ended <- struct{}{}
	}()
	go func() {
		request(s, "GET", fmt.Sprintf("/api/mobile/cli/sessions/%s?after=%d&wait=20", hosted, view.Revision), s.origin, "", true)
		ended <- struct{}{}
	}()
	time.Sleep(500 * time.Millisecond)
	closed := time.Now()
	s.beginClose()
	for range 2 {
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("a held request kept the closing server waiting")
		}
	}
	if elapsed := time.Since(closed); elapsed > 2*time.Second {
		t.Fatalf("held requests ended %v after the server closed", elapsed)
	}
}

func TestPhoneSessionListUsesTheResidentHelperForCodex(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	thread := uuid.NewString()
	writeTranscript(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "21", "rollout-2026-11-21T00-00-00-"+thread+".jsonl"),
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": thread, "cwd": guestPath(project)}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "codex title"}}}})
	s.ready = true
	list := func(query string) (int, sessionListAnswer) {
		t.Helper()
		code, answer := phoneList(t, s, query)
		if code == http.StatusOK && (len(answer.Sessions) != 2 || answer.Notice != "") {
			t.Fatalf("phone list = %+v", answer)
		}
		return code, answer
	}
	// The first lists install both helpers and start the resident one.
	for range 3 {
		list("")
	}
	warm := logins()
	started := time.Now()
	for range 3 {
		list("")
	}
	_, current := list("")
	t.Logf("warm phone list: %v per request; %d logins to warm up", time.Since(started)/4, warm)
	if logins() != warm {
		t.Fatalf("warm phone lists opened %d logins", logins()-warm)
	}
	if code, _ := list("?after=" + current.Revision); code != http.StatusNoContent {
		t.Fatalf("an unchanged phone list answered %d", code)
	}
	held := make(chan sessionListAnswer, 1)
	go func() {
		_, answer := phoneList(t, s, "?after="+current.Revision+"&wait=20")
		held <- answer
	}()
	time.Sleep(300 * time.Millisecond)
	if w := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/metadata", s.origin, `{"pinned":true}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case answer := <-held:
		if len(answer.Sessions) != 2 || !answer.Sessions[0].Pinned {
			t.Fatalf("held phone list = %+v", answer)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a held phone list missed a pin")
	}
	if logins() != warm {
		t.Fatalf("phone lists opened %d logins", logins()-warm)
	}
}

func TestOnlyListDiscoveryReusesTheAgentListing(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, claudeRuns := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	before, runs := logins(), claudeRuns()
	// Project removal acts on the running state, so it observes afresh.
	if err := s.discoverTerminalConversations(context.Background(), []string{project}); err != nil {
		t.Fatal(err)
	}
	if logins() != before+1 || claudeRuns() != runs+1 {
		t.Fatalf("a direct discovery reused the list's observation: %d logins, %d listings", logins()-before, claudeRuns()-runs)
	}
}

func TestStalledResidentHelperLeavesTimeForTheChannelHelper(t *testing.T) {
	previous := residentReadTimeout
	residentReadTimeout = time.Second
	t.Cleanup(func() { residentReadTimeout = previous })
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	// The next CLI listing stalls once.
	stall := "#!/bin/sh\n[ \"$1\" = agents ] || exit 64\nif [ -e \"$HOME/stall\" ]; then rm -f \"$HOME/stall\"; sleep 12; fi\nprintf '[]\\n'\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "claude"), []byte(stall), 0700); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	if err := os.WriteFile(filepath.Join(home, "stall"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// A new registered process makes the resident helper list again.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions", "1.json"), []byte(`{"pid":1,"sessionId":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	code, answer := sessionList(t, s, "")
	if code != http.StatusOK || len(answer.Sessions) != 1 || answer.Notice != "" {
		t.Fatalf("after a stalled resident helper: %d %+v", code, answer)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("the list took %v", elapsed)
	}
}
