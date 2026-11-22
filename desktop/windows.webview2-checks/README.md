# WebView2 checks for Claude Science host rendering

`Cage.WebView2Checks` measures, in the WebView2 runtime installed on a Windows
machine, the behaviour `desktop/windows/ScienceFrameHost.cs` relies on. It is a
test tool and is not shipped.

It opens one small window, serves a workbench document on `http://localhost:P/`
and a label document on `http://<32 hex>.localhost:Q/` (a dedicated worker that
starts a nested worker; every document and worker reports its language, time
zone, user agent and platform in a loop), and runs each case:

| case | teardown |
| --- | --- |
| `remove` | the frame element is removed |
| `remove-busy` | as `remove`, the frame's `pagehide` spins for 3 s |
| `crash` | `Page.crash` on the frame's session, then removal |
| `move` | `moveBefore` into a hidden place, then crash and removal |
| `hung` | the frame's main thread spins forever, then crash and removal |

For each case it records the DevTools target events (with the session they
arrived on), `ProcessFailed`, `Target.getTargets` on the page session before and
after, which guest sessions never reported a detach, and what each reporter
reported before and after the teardown.

A WebView2 cannot start in an SSH session (0x80070578), so `run-checks.ps1`
starts it in the signed-in user's session through a scheduled task:

```powershell
dotnet build -c Release desktop/windows.webview2-checks
./desktop/windows.webview2-checks/run-checks.ps1 -Dotnet <dotnet.exe> -Checks <bin>/Cage.WebView2Checks.dll -Results <dir>/results.json
```
