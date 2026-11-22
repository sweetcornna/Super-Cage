package workbench

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// The browser agent answers /audit with HTTP 400 and {"error": message} when
// the audit itself could not run (browser-agent.mjs request handler). On a
// starved guest the messages are a CDP command timeout or an unfinished
// evaluation. Those answers carry no time zone or language: they are not
// evidence, so the monitor retries inside the lease. A readable time zone or
// language that differs from the configuration is evidence and closes at once.
func TestBrowserAuditAnswersAreClassifiedByContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		closes bool
	}{
		{"CDP command timed out (the 08:19:04 incident)", http.StatusBadRequest, `{"error":"Browser command timed out: Target.createTarget"}`, false},
		{"settings page did not evaluate", http.StatusBadRequest, `{"error":"浏览器配置诊断未完成"}`, false},
		{"tab closed under the audit", http.StatusBadRequest, `{"error":"No target with given id found"}`, false},
		{"agent rejected the credentials", http.StatusUnauthorized, `{}`, false},
		{"unreadable answer", http.StatusOK, `not json`, false},
		{"readable time zone differs", http.StatusOK, `{"timezone":"Asia/Shanghai","expected_timezone":"Etc/UTC","language":"en-US","platform":"Linux x86_64"}`, true},
		{"readable language differs", http.StatusOK, `{"timezone":"Etc/UTC","expected_timezone":"Etc/UTC","language":"zh-CN","platform":"Linux x86_64"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			s.credentials.Token = "audit-token"
			audits := 0
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/audit" || r.Header.Get("Authorization") != "Bearer audit-token" {
					t.Errorf("unexpected agent request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var input struct {
					Network  bool   `json:"network"`
					Timezone string `json:"timezone"`
				}
				data, _ := io.ReadAll(r.Body)
				if json.Unmarshal(data, &input) != nil || input.Network || input.Timezone != cfg.Timezone {
					t.Errorf("monitor audit input: %s", data)
				}
				audits++
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer agent.Close()
			s.browserControl = localBrowserClient(t, s.lifetimeContext(), agent)
			// Only the system read is replaced; the browser read is the real one.
			previous := monitorGuestSettings
			t.Cleanup(func() { monitorGuestSettings = previous })
			monitorGuestSettings = func(*Server, context.Context, map[string]any) (guestObservation, error) {
				return matchingGuestSettings(cfg), nil
			}
			before := gate.Status()
			s.monitorSafetyWithProbe(exitProbeOK)
			after := gate.Status()
			if audits != 1 {
				t.Fatalf("audits: %d", audits)
			}
			if tc.closes {
				if after.Mode != cage.GateClosed || s.recovery.RunID != "" || !strings.Contains(s.safety.monitorError, "浏览器配置检查未通过") {
					t.Fatalf("a readable wrong value did not close at once: gate=%+v recovery=%+v error=%q", after, s.recovery, s.safety.monitorError)
				}
				return
			}
			if after.Mode != cage.GateOpen || !after.Until.Equal(before.Until) || after.Epoch != before.Epoch {
				t.Fatalf("an audit that could not run changed the lease: before=%+v after=%+v", before, after)
			}
			if e := s.safety.Explanation; e == nil || e.State != networkUnconfirmed || !strings.Contains(e.Reason, "读不到浏览器的时区和语言") {
				t.Fatalf("explanation: %+v", e)
			}
		})
	}
}
