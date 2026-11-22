# WebView2 checks for Claude Science host rendering

`Cage.WebView2Checks` measures, in the WebView2 runtime installed on a Windows
machine, the behaviour `desktop/windows/ScienceFrameHost.cs` relies on. It is a
test tool and is not shipped.

It opens one small window with the shell's switches
(`ScienceFrame.BackstopArguments`) and seeded preferences, serves a workbench
document on `http://localhost:P/` and label documents on
`http://<32 hex>.localhost:Q/` (a dedicated worker that starts a nested worker;
every document and worker reports its language, time zone, user agent and
platform in a loop), and drives the frame with the shell's own policy
(`ScienceFrame.Policy`, linked) and the host's order of actions. There is no
front, so the policy's front end is only recorded.

| case | what it does |
| --- | --- |
| `proxy` | the proxy proof's two loads (`.invalid`, then `127.0.0.3`) |
| `remove` | the frame element is removed (no crash): which detaches arrive |
| `remove-busy` | as `remove`, the frame's `pagehide` spins for 3 s: what its workers report afterwards |
| `teardown` | the policy's detach: crash, removal, every target gone, auto-attach off |
| `move` | `moveBefore` into a hidden place first (the pane's park), then the teardown |
| `hung` | the frame's main thread spins forever, then the teardown |
| `steady` | a frame kept for 15 s before the teardown |
| `storage` | the frame stores, the teardown clears its origin; a probe of that origin |
| `navigation` | a top-level navigation and a reload held and issued again |
| `frame-navigation` | the frame navigates itself to the workbench's origin |
| `locale-twice` | two frames of one origin: the locale override's error text |
| `two-labels` | two frames of two labels armed at once: separate processes, call errors |
| `kill-sweep` | the frame stores, the browser process is killed, a new WebView sweeps the journal (`ScienceFrameHost.Sweep`) |
| `host` | the shell's own `ScienceFrameHost` against a recording front: attach, detach, an unexpected frame exit, a held reload |

The `host` case runs last: a `ScienceFrameHost` stays subscribed to its
WebView for the WebView's life, as in `MainWindow`.

For each case it records the DevTools target events (with the session they
arrived on), `ProcessFailed`, navigation events, `Target.getTargets` on the page
session, and what each reporter reported before and after the teardown.

A WebView2 cannot start in an SSH session (0x80070578), so `run-checks.ps1`
starts it in the signed-in user's session through a scheduled task:

```powershell
dotnet build -c Release desktop/windows.webview2-checks
./desktop/windows.webview2-checks/run-checks.ps1 -Dotnet <dotnet.exe> -Checks <bin>/Cage.WebView2Checks.dll -Results <dir>/results.json
```
