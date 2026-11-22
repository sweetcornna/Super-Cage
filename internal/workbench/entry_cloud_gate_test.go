//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/cloudproxy"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/tenant"
)

// portalRequest is a GET as the portal's listener hands it over: the
// request target in origin form.
func portalRequest(host, path string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	return r
}

type gateRuntime struct{}

func (gateRuntime) Prepare(context.Context, tenant.LaunchSpec) (*tenant.Workspace, error) {
	return nil, errors.New("no runtime in this test")
}

// One gate, closed in every build (cloudentry): the gateway's /_cage/entry,
// the portal's device API, the workbench's cloud routes, the desktop's
// cloud row and the proxies' cloud mode open and close together.
func TestCloudEntryGatesMoveTogether(t *testing.T) {
	if cloudentry.Open() {
		t.Fatal("this build has the cloud entry open")
	}
	// The proxies read the user's paths: a home of the test's own.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	portal, err := tenant.NewServer(tenant.Config{
		Origin: "https://portal.example.test", StateDir: state, UI: fstest.MapFS{"index.html": {Data: []byte("portal")}},
		Runtime: gateRuntime{}, AccountVMLimit: 1, TotalVMLimit: 1, ParallelStarts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(portal.Close)
	vm := strings.Repeat("5a", 16)
	desktop := newCloudDesktop(t)
	bench := testServer(t)
	bench.tenantWorkspace = true
	observe := func() map[string]bool {
		entry := portalRequest("vm-"+vm+".portal.example.test", cloudentry.Path)
		entry.Header.Set("Connection", "Upgrade")
		entry.Header.Set("Upgrade", "websocket")
		entry.Header.Set("Sec-WebSocket-Version", "13")
		entry.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		gateway := httptest.NewRecorder()
		portal.ServeHTTP(gateway, entry)
		devices := httptest.NewRecorder()
		portal.ServeHTTP(devices, portalRequest("portal.example.test", "/api/tenant/devices"))
		var routes []string
		_ = json.Unmarshal(request(bench, "GET", "/api/agent/routes", "", "", true).Body.Bytes(), &routes)
		var out, cloudErr, attachErr bytes.Buffer
		cloudproxy.Main(context.Background(), []string{"entry", "proxy", cloudentry.Environment(vm)}, strings.NewReader(""), &out, &cloudErr)
		entryproxy.Main(context.Background(), []string{"proxy", cloudentry.Environment(vm)}, filepath.Join(home, "config.json"), nil, strings.NewReader(""), &out, &attachErr)
		return map[string]bool{
			"gateway /_cage/entry":           gateway.Code != http.StatusNotFound,
			"portal device API":              devices.Code != http.StatusNotFound,
			"workbench cloud ssh route":      slices.Contains(routes, "GET "+cloudentry.WorkbenchPath),
			"workbench cloud device route":   slices.Contains(routes, "POST "+cloudentry.DevicesPath),
			"desktop cloud row":              desktop.s.remote.context().CloudEntry,
			"cage-cloud-entry":               !strings.Contains(cloudErr.String(), cloudentry.ClosedMessage),
			"cage entry proxy (cage-attach)": !strings.Contains(attachErr.String(), cloudentry.ClosedMessage),
		}
	}
	for name, open := range observe() {
		if open {
			t.Errorf("%s is open with the gate closed", name)
		}
	}
	cloudentry.Lift(t)
	for name, open := range observe() {
		if !open {
			t.Errorf("%s stayed closed with the gate lifted", name)
		}
	}
}

// The cloud routes exist only in a cloud workspace's workbench, in a build
// with the cloud entry: never on a computer's own controller, the desktop
// gateway or a client listener.
func TestEntryCloudRoutesOnlyInATenantWorkspace(t *testing.T) {
	if cloudentry.WorkbenchPath != "/api/entry/cloud/ssh" || cloudentry.DevicesPath != "/api/entry/cloud/devices" {
		t.Fatal("entryCloudRoutes' literal patterns no longer match cloudentry")
	}
	for _, lifted := range []bool{false, true} {
		if lifted {
			cloudentry.Lift(t)
		}
		for _, listener := range []string{"workload", "tenant", "desktop", "client", "tenant+desktop", "tenant+client"} {
			s := testServer(t)
			s.tenantWorkspace = strings.HasPrefix(listener, "tenant")
			s.desktopEntry = strings.HasSuffix(listener, "desktop")
			s.clientOnly = strings.HasSuffix(listener, "client")
			var routes []string
			if err := json.Unmarshal(request(s, "GET", "/api/agent/routes", "", "", true).Body.Bytes(), &routes); err != nil {
				t.Fatal(err)
			}
			want := lifted && listener == "tenant"
			for _, route := range []string{"GET " + cloudentry.WorkbenchPath, "POST " + cloudentry.DevicesPath} {
				if slices.Contains(routes, route) != want {
					t.Errorf("gate lifted %v, %s listener: %s registered %v", lifted, listener, route, !want)
				}
			}
			// Whatever answers the path then, it is not the endpoint.
			if !want {
				w := request(s, "GET", cloudentry.WorkbenchPath, s.origin, "", true)
				if w.Header().Get(cloudentry.HostKeyHeader) != "" || strings.Contains(w.Body.String(), "device_unknown") {
					t.Errorf("%s listener answered the cloud endpoint: %d %s", listener, w.Code, w.Body)
				}
			}
		}
	}
}
