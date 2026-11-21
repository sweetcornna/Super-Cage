package workbench

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// guestReadFunctions are the monitoring and check reads of the guest, by
// the file that defines them.
var guestReadFunctions = map[string][]string{
	"safety.go":                {"guestObservation", "browserObservation"},
	"windows_safety.go":        {"windowsGuestObservation", "decodeWindowsObservation"},
	"windows_remote_safety.go": {"remoteWindowsObservation"},
	"macos_safety.go":          {"macOSGuestObservation", "macOSBrowserObservation", "recheckMacOSBrowserSettings"},
}

// guestReadSite is one place a read returns an error, with its class:
// "no" (noAnswer: retried within the lease), "wrong" (wrongAnswer: closes at
// once) or "passes" (returns another read's classified error unchanged).
// read, when set, makes the real read fail at this site.
type guestReadSite struct {
	function, class, what string
	read                  func(*testing.T) error
}

func windowsDecodeError(data string) func(*testing.T) error {
	return func(*testing.T) error {
		_, err := decodeWindowsObservation([]byte(data))
		return err
	}
}

func windowsReadError(remote bool, scenario, policy string) func(*testing.T) error {
	return func(t *testing.T) error { return windowsSettingsReadError(t, remote, scenario, policy) }
}

func macOSRecheckError(reply string) func(*testing.T) error {
	return func(t *testing.T) error {
		s := testServer(t)
		s.macosGuest = &guestos.MacOSProfile{}
		s.windowsBrowserControl = scriptedBrowserClient(t, reply)
		return s.recheckMacOSBrowserSettings(context.Background(), cage.DefaultConfig())
	}
}

// Every error site of every guest read, with its class. A new site must be
// added here, which makes its author decide which class it is.
var guestReadSites = []guestReadSite{
	{"guestObservation", "passes", "a Windows guest's read", nil},
	{"guestObservation", "passes", "a macOS guest's read", nil},
	{"guestObservation", "no", "the probe's input does not encode", nil},
	{"guestObservation", "no", "the guest control channel failed, timed out or was killed", nil},
	{"guestObservation", "no", "the probe's reply is not JSON (killed while writing)", nil},
	{"browserObservation", "passes", "a macOS guest's page read", nil},
	{"browserObservation", "no", "the audit failed or its reply is unreadable", nil},

	{"windowsGuestObservation", "passes", "a cloud sandbox's read", nil},
	{"windowsGuestObservation", "no", "the configuration is unreadable", nil},
	{"windowsGuestObservation", "no", "the relay command is unavailable", nil},
	{"windowsGuestObservation", "no", "the probe's input does not encode", nil},
	{"windowsGuestObservation", "no", "the relay could not carry the probe", windowsReadError(false, "relay-down", "")},
	{"windowsGuestObservation", "passes", "the reply's own classes", nil},
	{"decodeWindowsObservation", "no", "the reply is not JSON", windowsDecodeError("not json")},
	{"decodeWindowsObservation", "wrong", "the guest reported a failed stage", windowsDecodeError(`{"error":"safety_probe_failed","stage":"environment","error_code":"access_denied"}`)},
	{"decodeWindowsObservation", "no", "the reply is not one observation", windowsDecodeError(`{"tz":1}`)},
	{"decodeWindowsObservation", "wrong", "an empty setting or a locale that cannot load", windowsDecodeError(`{"tz":"Etc/UTC","system_timezone":"Etc/UTC","process_locale":"en-US","system_locale":"en-US","locale_valid":false}`)},

	{"remoteWindowsObservation", "no", "the configuration is unreadable", nil},
	{"remoteWindowsObservation", "no", "the relay command is unavailable", nil},
	{"remoteWindowsObservation", "no", "the probe's input does not encode", nil},
	{"remoteWindowsObservation", "no", "the relay could not carry the probe", windowsReadError(true, "relay-down", "")},
	{"remoteWindowsObservation", "passes", "decodeWindowsObservation's classes", windowsReadError(true, "stage-failed", "")},
	{"remoteWindowsObservation", "wrong", "not the expected AppContainer identity", windowsReadError(true, "identity-missing", "")},
	{"remoteWindowsObservation", "wrong", "the policy tool found a filter missing or in conflict", windowsReadError(true, "valid", "missing")},
	{"remoteWindowsObservation", "no", "the policy tool could not run or was stopped", nil},
	{"remoteWindowsObservation", "no", "the policy readback is unreadable", windowsReadError(true, "valid", "garbled")},
	{"remoteWindowsObservation", "wrong", "the policy does not match the sandbox", windowsReadError(true, "valid", "mismatch")},

	{"macOSGuestObservation", "no", "the probe's input does not encode", nil},
	{"macOSGuestObservation", "no", "the guest control channel failed or the reply is unreadable", nil},
	{"macOSBrowserObservation", "no", "the page could not be read (full check only)", nil},
	{"recheckMacOSBrowserSettings", "wrong", "the browser service says Chrome changed or has no sample", macOSRecheckError(`{"error":"x","code":"browser_recheck_needed"}`)},
	{"recheckMacOSBrowserSettings", "no", "no reply or a failure without that verdict", macOSRecheckError(`{"error":"x","code":"browser_operation_failed"}`)},
	{"recheckMacOSBrowserSettings", "no", "an unreadable reply", macOSRecheckError("not json")},
	{"recheckMacOSBrowserSettings", "wrong", "a reply that says the settings changed", macOSRecheckError(`{"unchanged":false}`)},
}

func TestGuestReadErrorSites(t *testing.T) {
	scanned := map[string]bool{}
	for _, names := range guestReadFunctions {
		for _, name := range names {
			scanned[name] = true
		}
	}
	got := map[string]map[string]int{}
	count := func(function, class string) {
		if got[function] == nil {
			got[function] = map[string]int{}
		}
		got[function][class]++
	}
	fset := token.NewFileSet()
	for file, names := range guestReadFunctions {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !scanned[fn.Name.Name] {
				continue
			}
			found[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 {
					return true
				}
				last := ret.Results[len(ret.Results)-1]
				if id, ok := last.(*ast.Ident); ok && id.Name == "nil" {
					return true
				}
				if call, ok := last.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "noAnswer" || id.Name == "wrongAnswer") {
						count(fn.Name.Name, map[string]string{"noAnswer": "no", "wrongAnswer": "wrong"}[id.Name])
						return true
					}
					// return s.otherRead(...), or decodeWindowsObservation(...)
					callee := ""
					switch f := call.Fun.(type) {
					case *ast.Ident:
						callee = f.Name
					case *ast.SelectorExpr:
						callee = f.Sel.Name
					}
					if len(ret.Results) == 1 && scanned[callee] {
						count(fn.Name.Name, "passes")
						return true
					}
				}
				// The one reviewed pass-through of a variable: the cloud read
				// returns decodeWindowsObservation's error as it came.
				if id, ok := last.(*ast.Ident); ok && id.Name == "err" && fn.Name.Name == "remoteWindowsObservation" {
					count(fn.Name.Name, "passes")
					return true
				}
				var text bytes.Buffer
				_ = printer.Fprint(&text, fset, ret)
				t.Errorf("%s: %s returns an unclassified error; wrap it in noAnswer or wrongAnswer and list it in guestReadSites", fset.Position(ret.Pos()), text.String())
				return true
			})
		}
		for _, name := range names {
			if !found[name] {
				t.Errorf("%s no longer defines %s", file, name)
			}
		}
	}
	want := map[string]map[string]int{}
	for _, site := range guestReadSites {
		if want[site.function] == nil {
			want[site.function] = map[string]int{}
		}
		want[site.function][site.class]++
	}
	for function := range scanned {
		for _, class := range []string{"no", "wrong", "passes"} {
			if got[function][class] != want[function][class] {
				t.Errorf("%s has %d %q error sites in the source and %d in guestReadSites", function, got[function][class], class, want[function][class])
			}
		}
	}
	for _, site := range guestReadSites {
		if site.read == nil {
			continue
		}
		t.Run(site.function+"/"+site.what, func(t *testing.T) {
			err := site.read(t)
			if err == nil {
				t.Fatal("the read did not fail")
			}
			class := "wrong"
			if unanswered(err) {
				class = "no"
			}
			if site.class != "passes" && class != site.class {
				t.Fatalf("%v is %q, want %q", err, class, site.class)
			}
		})
	}
}
