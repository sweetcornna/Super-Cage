package agentcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestUpgradeHelpWithoutConnection(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"upgrade", "--help"}, strings.NewReader(""), &out); code != 0 || !strings.Contains(out.String(), "cage agent upgrade [AGENT_ID]") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

func TestSelectUpgrade(t *testing.T) {
	catalog := json.RawMessage(`{"ready":true,"vm_running":true,"agents":[
		{"spec":{"id":"absent","channels":[{"id":"npm","kind":"npm"}]}},
		{"runtime":{"installed":true},"spec":{"id":"external","channels":[{"id":"path","kind":"existing"}]}},
		{"runtime":{"installed":true,"version":"1.0.0"},"spec":{"id":"codex","name":"Codex","channels":[{"id":"one","kind":"npm"},{"id":"preferred","kind":"script","label":"Preferred channel"}]},"config":{"preferred_channel":"preferred"}}
	]}`)
	for _, tc := range []struct {
		id, input       string
		submit, invalid bool
	}{
		{"", "1\n", true, false}, {"", "codex\n", true, false}, {"codex", "", true, false},
		{"", "", false, false}, {"", "9\n", false, true}, {"external", "", false, true},
	} {
		var menu bytes.Buffer
		body, err := selectUpgrade(catalog, tc.id, strings.NewReader(tc.input), &menu)
		if (err != nil) != tc.invalid || (body != nil) != tc.submit {
			t.Fatalf("%+v: body=%s err=%v", tc, body, err)
		}
		if tc.submit && string(body) != `{"agent_id":"codex","channel":"preferred"}` {
			t.Fatal(string(body))
		}
		if tc.id == "" && (!strings.Contains(menu.String(), "Codex (codex) 1.0.0") || strings.Contains(menu.String(), "external")) {
			t.Fatal(menu.String())
		}
	}
}
