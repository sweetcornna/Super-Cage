package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// The test binary stands in for the Windows relay (safety, headless-safety)
// and the remote network policy tool, answering as the scenario says.
func init() {
	if os.Getenv("CAGE_SAFETY_RELAY_TEST") != "1" {
		return
	}
	scenario := os.Getenv("CAGE_SAFETY_RELAY_SCENARIO")
	if len(os.Args) == 5 && os.Args[1] == "remote-network-policy" && os.Args[2] == "broker-status" {
		switch os.Getenv("CAGE_SAFETY_POLICY_SCENARIO") {
		case "missing": // The tool throws when the sandbox filters are gone.
			fmt.Fprintln(os.Stderr, "sandbox_network_policy_missing")
			os.Exit(1)
		case "mismatch":
			fmt.Println(`{"version":1,"package":"S-1-15-2-7","broker":"127.0.0.1:17897","broker_filter":false,"loopback_exemption":true,"udp_bind_filters":2}`)
		case "garbled":
			fmt.Println("not json")
		default:
			fmt.Println(`{"version":1,"package":"S-1-15-2-7","broker":"127.0.0.1:17897","broker_filter":true,"loopback_exemption":true,"udp_bind_filters":2}`)
		}
		os.Exit(0)
	}
	if len(os.Args) != 4 || os.Args[1] != "--relay-profile" || (os.Args[3] != "safety" && os.Args[3] != "headless-safety") {
		os.Exit(2)
	}
	var input map[string]any
	_ = json.NewDecoder(os.Stdin).Decode(&input)
	remote := `,"remote":{"sandbox_sid":"S-1-15-2-7","session":"headless-1","token_verified":true,"nonce":"n"}`
	switch scenario {
	case "relay-down":
		os.Exit(1)
	case "locale-invalid":
		fmt.Println(`{"tz":"Etc/UTC","system_timezone":"Etc/UTC","process_locale":"en-US","system_locale":"","locale_valid":false}`)
	case "empty-timezone":
		fmt.Println(`{"tz":"","system_timezone":"Etc/UTC","process_locale":"en-US","system_locale":"en-US","locale_valid":true}`)
	case "stage-failed":
		fmt.Println(`{"error":"safety_probe_failed","stage":"environment","error_code":"access_denied","native_code":0}`)
	case "garbled":
		fmt.Println("not json")
	case "identity-missing":
		fmt.Println(`{"tz":"Etc/UTC","system_timezone":"Etc/UTC","process_locale":"en-US","system_locale":"en-US","locale_valid":true,"remote":{"sandbox_sid":"S-1-5-21-1","session":"headless-1","token_verified":true}}`)
	default:
		fmt.Println(`{"tz":"Etc/UTC","system_timezone":"Etc/UTC","process_locale":"en-US","system_locale":"en-US","locale_valid":true` + remote + `}`)
	}
	os.Exit(0)
}

// bindTestWindowsRelay binds a Windows guest whose relay is this test binary.
func bindTestWindowsRelay(t *testing.T, s *Server, remote bool, scenario, policy string) {
	t.Helper()
	binding := guestos.WindowsProfile{Version: 1, VMID: uuid.New(), GUIUserSID: "S-1-5-21-1-2-3-1001", GUIUser: "Guest",
		InstanceDirectory: filepath.Join(s.profile, "windows-vm"), PairingFile: filepath.Join(s.profile, "pair.dpapi"), RelayExecutable: os.Args[0]}
	if remote {
		binding.Mode, binding.MachineID = "remote", uuid.NewString()
	}
	if err := guestos.BindWindowsProfile(s.profile, binding); err != nil {
		t.Fatal(err)
	}
	s.windowsGuest = &binding
	t.Setenv("CAGE_SAFETY_RELAY_TEST", "1")
	t.Setenv("CAGE_SAFETY_RELAY_SCENARIO", scenario)
	t.Setenv("CAGE_SAFETY_POLICY_SCENARIO", policy)
}

// windowsSettingsReadError is what the real Windows read returns for a
// scenario: the local relay, or the cloud sandbox and its policy readback.
func windowsSettingsReadError(t *testing.T, remote bool, scenario, policy string) error {
	t.Helper()
	s := testServer(t)
	bindTestWindowsRelay(t, s, remote, scenario, policy)
	_, err := s.windowsGuestObservation(context.Background(), map[string]any{"stage": "environment"})
	return err
}

func TestWrongWindowsAnswersCloseTheGateAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name             string
		remote           bool
		scenario, policy string
	}{
		{"locale cannot load", false, "locale-invalid", ""},
		{"empty time zone", false, "empty-timezone", ""},
		{"guest reports a failed stage", false, "stage-failed", ""},
		{"cloud: not the expected AppContainer", true, "identity-missing", ""},
		{"cloud: sandbox filters gone", true, "valid", "missing"},
		{"cloud: broker filter missing", true, "valid", "mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := windowsSettingsReadError(t, tc.remote, tc.scenario, tc.policy)
			if readErr == nil {
				t.Fatal("the read accepted a wrong answer")
			}
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) { return guestObservation{}, readErr },
				func(context.Context) (browserObservation, error) { return matchingBrowserSettings(cfg), nil })
			s.monitorSafetyWithProbe(exitProbeOK)
			if state := gate.Status(); state.Mode != cage.GateClosed {
				t.Fatalf("a wrong answer (%v) left the gate open: %+v", readErr, state)
			}
			if s.safety.deferral.RunID != "" {
				t.Fatalf("a wrong answer was deferred: %+v", s.safety.deferral)
			}
		})
	}
}

func TestUnansweredWindowsReadsAreRetriedWithinTheLease(t *testing.T) {
	for _, tc := range []struct {
		name             string
		remote           bool
		scenario, policy string
	}{
		{"relay could not reach the guest", false, "relay-down", ""},
		{"unreadable reply", false, "garbled", ""},
		{"cloud: sandbox relay could not reach the guest", true, "relay-down", ""},
		{"cloud: unreadable policy readback", true, "valid", "garbled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := windowsSettingsReadError(t, tc.remote, tc.scenario, tc.policy)
			if readErr == nil {
				t.Fatal("the read reported an answer")
			}
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) { return guestObservation{}, readErr },
				func(context.Context) (browserObservation, error) { return matchingBrowserSettings(cfg), nil })
			before := gate.Status()
			s.monitorSafetyWithProbe(exitProbeOK)
			if state := gate.Status(); state.Mode != cage.GateOpen || !state.Until.Equal(before.Until) || s.safety.deferral.RunID == "" {
				t.Fatalf("an unanswered read (%v) was not retried within the lease: %+v deferral=%+v", readErr, state, s.safety.deferral)
			}
		})
	}
}

func TestUnansweredMacOSBrowserRecheckIsRetriedWithinTheLease(t *testing.T) {
	// macOS gets the same tolerance at the browser step as the other guests:
	// a recheck the browser agent did not answer in time is retried within
	// the lease; one that says Chrome changed still closes at once.
	for _, tc := range []struct {
		name  string
		reply string
		open  bool
	}{
		{"agent did not answer", "", true},
		{"unreadable reply", "not json", true},
		{"agent failed without a verdict", `{"error":"客机浏览器操作失败","code":"browser_operation_failed"}`, true},
		{"Chrome changed", `{"error":"Chrome 进程或浏览器设置发生变化，请重新检测","code":"browser_recheck_needed"}`, false},
		{"Chrome unchanged", `{"unchanged":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			// The same instance directory, so the monitor finds the gate.
			s.macosGuest = &guestos.MacOSProfile{Instance: cage.InstanceName(s.profile, s.environmentDir())}
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) {
				return guestObservation{TZ: cfg.Timezone, SystemTimezone: cfg.Timezone, ProcessLocale: cfg.Locale, SystemLocale: cfg.Locale, SystemLanguage: cfg.Locale}, nil
			}, func(context.Context) (browserObservation, error) {
				return browserObservation{}, errors.New("unexpected")
			})
			s.windowsBrowserControl = scriptedBrowserClient(t, tc.reply)
			before := gate.Status()
			for !time.Now().Add(cage.GateLease).After(before.Until) {
				time.Sleep(time.Millisecond)
			}
			s.monitorSafetyWithProbe(exitProbeOK)
			state := gate.Status()
			switch {
			case !tc.open && state.Mode != cage.GateClosed:
				t.Fatalf("a changed Chrome left the gate open: %+v", state)
			case tc.open && state.Mode != cage.GateOpen:
				t.Fatalf("the gate closed: %+v (error %q)", state, s.safety.Error)
			case tc.reply == `{"unchanged":true}` && !state.Until.After(before.Until):
				t.Fatalf("a confirmed round did not renew: %+v", state)
			case tc.open && tc.reply != `{"unchanged":true}` && (!state.Until.Equal(before.Until) || s.safety.deferral.Stage != "browser_settings"):
				t.Fatalf("an unanswered recheck was not deferred: %+v deferral=%+v", state, s.safety.deferral)
			}
		})
	}
}

// scriptedBrowserClient is a native browser connection whose guest side
// answers every request with reply ("" closes without answering). The guest
// side is this test binary (see init), so it runs on every host.
func scriptedBrowserClient(t *testing.T, reply string) *windowsBrowserClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &windowsBrowserClient{ctx: ctx, cancel: cancel, start: func(ctx context.Context) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "CAGE_SCRIPTED_BROWSER_TEST=1", "CAGE_SCRIPTED_BROWSER_REPLY="+reply)
		return cmd, nil
	}}
}

// The guest side of scriptedBrowserClient.
func init() {
	if os.Getenv("CAGE_SCRIPTED_BROWSER_TEST") != "1" {
		return
	}
	reply := os.Getenv("CAGE_SCRIPTED_BROWSER_REPLY")
	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		if reply == "" {
			os.Exit(0)
		}
		fmt.Println(reply)
	}
	os.Exit(0)
}
