using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace Cage.Windows;

// Cage.WebView2Checks <results.json> <user data folder>: measures, in the
// installed WebView2 runtime, what Claude Science host rendering assumes
// (README.md lists the cases). It opens one small window, runs every case
// with the shell's switches and its frame policy (ScienceFrame.cs), writes
// the results and closes the window.
internal static class Program
{
    [STAThread]
    private static int Main(string[] args)
    {
        if (args.Length < 2) return 2;
        ApplicationConfiguration.Initialize();
        using var form = new ChecksForm(args[0], args[1]);
        Application.Run(form);
        return form.ExitCode;
    }
}

internal sealed class ChecksForm : Form
{
    private readonly string output, data;
    private WebView2 web = new() { Dock = DockStyle.Fill };
    private readonly Stopwatch clock = Stopwatch.StartNew();
    private readonly object gate = new();
    private readonly JsonArray events = [], reports = [], failures = [], callErrors = [];
    private readonly JsonObject results = new();
    private CoreWebView2 core = null!;
    private ScienceFrame.Policy policy = new();
    private string current = "", frameSession = "";
    private readonly Dictionary<string, TaskCompletionSource> crashWaits = new(StringComparer.Ordinal);
    private readonly HashSet<ulong> held = [];
    private string heldUri = "";
    private bool heldReload, holdNavigations;
    private long pageDetachedAt = -1;
    private LoopbackServer workbench = null!, labels = null!;
    public int ExitCode { get; private set; } = 1;

    // The guest identity the checks give the frame (the golden test identity).
    private static readonly JsonObject Identity = new()
    {
        ["timezone"] = "Asia/Tokyo",
        ["locale"] = "ja-JP",
        ["accept_language"] = "ja-JP",
        ["user_agent"] = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36",
        ["platform"] = "Linux x86_64",
        ["ua_metadata"] = new JsonObject
        {
            ["brands"] = new JsonArray(new JsonObject { ["brand"] = "Chromium", ["version"] = "157" }),
            ["fullVersionList"] = new JsonArray(),
            ["platform"] = "Linux", ["platformVersion"] = "", ["architecture"] = "x86", ["model"] = "", ["mobile"] = false, ["bitness"] = "64",
        },
    };

    public ChecksForm(string output, string data)
    {
        this.output = output;
        this.data = data;
        Text = "Cage WebView2 checks";
        StartPosition = FormStartPosition.Manual;
        Location = new Point(40, 40);
        Size = new Size(520, 380);
        Controls.Add(web);
        Shown += async (_, _) => await Run();
    }

    private long Now => clock.ElapsedMilliseconds;

    private void Event(string kind, JsonObject detail)
    {
        detail["t"] = Now;
        detail["kind"] = kind;
        lock (gate) events.Add(detail);
    }

    private const string Reporter = """
        const report = (from, extra) => fetch("/report", { method: "POST", body: JSON.stringify({ from, lang: navigator.language, langs: navigator.languages, tz: Intl.DateTimeFormat().resolvedOptions().timeZone, ua: navigator.userAgent, platform: navigator.platform, ...extra }) });
        const loop = async (from) => { for (;;) { try { await report(from); } catch {} await new Promise((resolve) => setTimeout(resolve, 25)); } };
        """;

    private const string Workbench = "<!doctype html><meta charset=\"utf-8\"><title>workbench</title><div id=\"pane\"></div><script>" + Reporter + "report('workbench');</script>";

    private const string Store = """
        localStorage.setItem("env", "kept");
        (async () => {
          const db = await new Promise((resolve, reject) => { const open = indexedDB.open("science", 1); open.onupgradeneeded = () => open.result.createObjectStore("s"); open.onsuccess = () => resolve(open.result); open.onerror = () => reject(open.error); });
          await new Promise((resolve) => { const tx = db.transaction("s", "readwrite"); tx.objectStore("s").put("x".repeat(200000), "k"); tx.oncomplete = resolve; });
          const cache = await caches.open("science");
          await cache.put("/kept", new Response("y".repeat(100000)));
          report("stored");
        })();
        """;

    private const string Probe = """
        (async () => report("probe", { storage: JSON.stringify({ local: localStorage.getItem("env"), databases: (await indexedDB.databases()).map(d => d.name), caches: await caches.keys() }) }))();
        """;

    private string LabelPage(string query, int workbenchPort)
    {
        var script = Reporter;
        if (query.Contains("probe")) return "<!doctype html><title>probe</title><script>" + script + Probe + "</script>";
        script += "loop('doc'); new Worker('/direct.js');";
        if (query.Contains("busy")) script += "addEventListener('pagehide', () => { const t = Date.now(); while (Date.now() - t < 3000) {} });";
        if (query.Contains("store")) script += Store;
        if (query.Contains("escape")) script += "setTimeout(() => { location.href = 'http://localhost:" + workbenchPort + "/escape'; }, 300);";
        return "<!doctype html><meta charset=\"utf-8\"><title>label</title><script>" + script + "</script>";
    }

    private LoopbackServer.Response Serve(LoopbackServer.Request request, bool label)
    {
        var parts = request.Path.Split('?', 2);
        var path = parts[0];
        var query = parts.Length > 1 ? parts[1] : "";
        Event("request", new JsonObject { ["host"] = request.Host, ["path"] = request.Path });
        if (path == "/report" && request.Method == "POST")
        {
            JsonObject? body = null;
            try { body = JsonNode.Parse(request.Body) as JsonObject; } catch (JsonException) { }
            body ??= new JsonObject();
            body["t"] = Now;
            body["host"] = request.Host;
            body["accept_language"] = request.Headers.GetValueOrDefault("Accept-Language", "");
            lock (gate) reports.Add(body);
            return new(204, "text/plain", "");
        }
        if (!label && path is "/" or "/next") return new(200, "text/html; charset=utf-8", Workbench);
        if (label && path == "/") return new(200, "text/html; charset=utf-8", LabelPage(query, workbench.Port));
        if (label && path == "/direct.js") return new(200, "text/javascript", Reporter + "new Worker('/nested.js'); loop('direct');");
        if (label && path == "/nested.js") return new(200, "text/javascript", Reporter + "loop('nested');");
        return new(404, "text/plain", "not found");
    }

    private async Task Run()
    {
        using var watchdog = new System.Windows.Forms.Timer { Interval = 300000 };
        watchdog.Tick += (_, _) => { results["watchdog"] = true; Finish(); };
        watchdog.Start();
        try
        {
            results["runtime_available"] = CoreWebView2Environment.GetAvailableBrowserVersionString();
            results["arguments"] = ScienceFrame.BackstopArguments;
            results["preferences_seeded"] = ScienceFrame.SeedPreferences(data, true);
            workbench = new LoopbackServer(request => Serve(request, false));
            labels = new LoopbackServer(request => Serve(request, true));
            await Start();
            results["runtime"] = core.Environment.BrowserVersionString;
            var cases = new JsonObject();
            foreach (var (name, run) in new (string, Func<Task<JsonObject>>)[] {
                ("proxy", ProxyCase), ("remove", () => FrameCase("remove", "")), ("remove-busy", () => FrameCase("remove-busy", "busy")),
                ("teardown", () => FrameCase("teardown", "")), ("move", () => FrameCase("move", "")), ("hung", () => FrameCase("hung", "")),
                ("steady", () => FrameCase("steady", "")), ("storage", StorageCase), ("navigation", NavigationCase),
                ("frame-navigation", () => FrameCase("frame-navigation", "escape")), ("locale-twice", LocaleTwiceCase), ("kill-sweep", KillSweepCase),
            })
            {
                try { cases[name] = await run(); }
                catch (Exception error) { cases[name] = new JsonObject { ["error"] = error.ToString() }; }
            }
            results["cases"] = cases;
            var preferences = ScienceFrame.PreferencesPath(data);
            results["preferences_after"] = File.Exists(preferences) ? JsonNode.Parse(File.ReadAllText(preferences))?["net"]?.DeepClone() : "missing";
            ExitCode = 0;
        }
        catch (Exception error)
        {
            results["error"] = error.ToString();
        }
        Finish();
    }

    private async Task Start()
    {
        var environment = await CoreWebView2Environment.CreateAsync(null, data, new CoreWebView2EnvironmentOptions(ScienceFrame.BackstopArguments));
        await web.EnsureCoreWebView2Async(environment);
        core = web.CoreWebView2;
        policy = new ScienceFrame.Policy();
        core.ProcessFailed += (_, e) =>
        {
            var frames = new JsonArray();
            try { foreach (var frame in e.FrameInfosForFailedProcess ?? []) frames.Add(new JsonObject { ["name"] = frame.Name, ["source"] = frame.Source }); }
            catch (Exception error) { frames.Add(error.GetType().Name); }
            Event("ProcessFailed", new JsonObject { ["process_kind"] = e.ProcessFailedKind.ToString(), ["reason"] = e.Reason.ToString(), ["exit_code"] = e.ExitCode, ["frames"] = frames });
        };
        foreach (var method in new[] { "Target.attachedToTarget", "Target.detachedFromTarget", "Inspector.targetCrashed" })
            core.GetDevToolsProtocolEventReceiver(method).DevToolsProtocolEventReceived += (_, e) => Received(method, e);
        core.NavigationStarting += (_, e) =>
        {
            Event("NavigationStarting", new JsonObject { ["uri"] = e.Uri, ["navigation_kind"] = e.NavigationKind.ToString(), ["id"] = e.NavigationId, ["redirected"] = e.IsRedirected });
            if (!holdNavigations) return;
            var ending = current;
            current = "";
            var actions = policy.Navigate();
            if (actions.Any(action => action.Proceed))
            {
                _ = Apply(actions.Where(action => !action.Proceed).ToList(), ending);
                return;
            }
            heldUri = e.Uri;
            heldReload = e.NavigationKind == CoreWebView2NavigationKind.Reload;
            e.Cancel = true;
            held.Add(e.NavigationId);
            _ = Apply(actions, ending);
        };
        core.NavigationCompleted += (_, e) => Event("NavigationCompleted", new JsonObject { ["id"] = e.NavigationId, ["success"] = e.IsSuccess, ["status"] = e.WebErrorStatus.ToString(), ["held"] = held.Contains(e.NavigationId) });
        core.FrameNavigationStarting += (_, e) =>
        {
            var allowed = e.Uri is "about:blank" or "about:srcdoc" || current.Length != 0 && Uri.TryCreate(e.Uri, UriKind.Absolute, out var url) && url.GetLeftPart(UriPartial.Authority) == current;
            if (!allowed) e.Cancel = true;
            Event("FrameNavigationStarting", new JsonObject { ["uri"] = e.Uri, ["cancelled"] = !allowed });
        };
    }

    // Open navigates the window to the workbench and arms the policy.
    private async Task Open(string path = "/")
    {
        await LoadWorkbench(path);
        await Apply(policy.Arm(Identity), "");
    }

    private async Task LoadWorkbench(string path)
    {
        var navigated = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        void Completed(object? sender, CoreWebView2NavigationCompletedEventArgs e) => navigated.TrySetResult(e.IsSuccess);
        core.NavigationCompleted += Completed;
        core.Navigate("http://localhost:" + workbench.Port + path);
        await navigated.Task.WaitAsync(TimeSpan.FromSeconds(15));
        core.NavigationCompleted -= Completed;
    }

    // Frame adds a frame element for a new label (a new session origin).
    private async Task<string> Frame(string query)
    {
        var host = Guid.NewGuid().ToString("N") + ".localhost:" + labels.Port;
        current = "http://" + host;
        await core.ExecuteScriptAsync("(() => { const f = document.createElement('iframe'); f.dataset.scienceFrame = ''; f.setAttribute('sandbox', 'allow-scripts allow-same-origin allow-forms'); f.style.cssText = 'width:300px;height:120px;border:0'; f.src = " +
            JsonSerializer.Serialize("http://" + host + "/" + (query.Length == 0 ? "" : "?" + query)) + "; (document.getElementById('pane') || document.body).append(f); return true; })()");
        return host;
    }

    private Task Teardown()
    {
        var ending = current;
        current = "";
        return Apply(policy.Detach(), ending);
    }

    private async Task<JsonObject> FrameCase(string name, string query)
    {
        var record = new JsonObject();
        var start = Now;
        await Open();
        var host = await Frame(query);
        record["host"] = host;
        record["all_reporting"] = await Until(10000, () => new[] { "doc", "direct", "nested" }.All(from => Reports(host).Any(report => report["from"]?.GetValue<string>() == from)));
        await Task.Delay(name == "steady" ? 15000 : 500);
        record["targets_before"] = await Targets();
        lock (gate) record["frame_session"] = frameSession;
        var teardown = Now;
        record["teardown"] = teardown;
        switch (name)
        {
            case "remove":
            case "remove-busy":
                await core.ExecuteScriptAsync(ScienceFrame.RemoveFrame(""));
                await Task.Delay(7000);
                record["policy_attached"] = policy.Attached;
                break;
            case "move":
                record["moved"] = await core.ExecuteScriptAsync("(() => { const f = document.querySelector('iframe[data-science-frame]'); if (!document.body.moveBefore) return 'no moveBefore'; f.hidden = true; document.body.moveBefore(f, null); document.getElementById('pane').remove(); return 'moved ' + f.isConnected; })()");
                await Task.Delay(1500);
                record["detached_while_parked"] = Since(teardown).Count(item => item?["kind"]?.GetValue<string>() == "Target.detachedFromTarget");
                record["crash_at"] = Now;
                await Teardown();
                break;
            case "hung":
                _ = core.CallDevToolsProtocolMethodForSessionAsync(frameSession, "Runtime.evaluate", new JsonObject { ["expression"] = "while (true) {}" }.ToJsonString());
                await Task.Delay(500);
                record["crash_at"] = Now;
                await Teardown();
                break;
            case "frame-navigation":
                await Task.Delay(1500);
                await Teardown();
                break;
            default:
                await Teardown();
                break;
        }
        record["page_detach"] = await Until(6000, () => !policy.Attached) ? pageDetachedAt - teardown : -1;
        await Task.Delay(1500);
        record["targets_after"] = await Targets();
        record["reports"] = Summary(host, teardown);
        record["events"] = Since(start);
        return record;
    }

    private async Task<JsonObject> StorageCase()
    {
        var record = new JsonObject();
        var start = Now;
        await Open();
        var host = await Frame("store");
        record["stored"] = await Until(10000, () => Reports(host).Any(report => report["from"]?.GetValue<string>() == "stored"));
        var teardown = Now;
        await Teardown();
        record["page_detach"] = await Until(6000, () => !policy.Attached) ? pageDetachedAt - teardown : -1;
        await Task.Delay(1000);
        record["left"] = await ProbeStorage(host);
        record["events"] = Since(start);
        return record;
    }

    // ProbeStorage loads a document of the session origin under the same
    // window (the same storage partition), unarmed, and returns what it found.
    private async Task<string> ProbeStorage(string host)
    {
        await core.ExecuteScriptAsync("(() => { const f = document.createElement('iframe'); f.src = " + JsonSerializer.Serialize("http://" + host + "/?probe") + "; document.body.append(f); return true; })()");
        await Until(10000, () => Reports(host).Any(report => report["from"]?.GetValue<string>() == "probe"));
        return Reports(host).LastOrDefault(report => report["from"]?.GetValue<string>() == "probe")?["storage"]?.GetValue<string>() ?? "no answer";
    }

    private async Task<JsonObject> NavigationCase()
    {
        var record = new JsonObject();
        var start = Now;
        await Open();
        var host = await Frame("");
        await Until(10000, () => Reports(host).Any(report => report["from"]?.GetValue<string>() == "nested"));
        holdNavigations = true;
        var teardown = Now;
        await core.ExecuteScriptAsync("location.href = '/next'; true");
        record["went"] = await Until(8000, () => Since(teardown).Any(item => item?["kind"]?.GetValue<string>() == "NavigationCompleted" && item["success"]?.GetValue<bool>() == true));
        record["reports_after"] = Summary(host, teardown);
        // A reload while a frame is open: held, then issued again as a reload.
        await Apply(policy.Arm(Identity), "");
        var second = await Frame("");
        await Until(10000, () => Reports(second).Any(report => report["from"]?.GetValue<string>() == "nested"));
        var reload = Now;
        await core.ExecuteScriptAsync("location.reload(); true");
        record["reloaded"] = await Until(8000, () => Since(reload).Any(item => item?["kind"]?.GetValue<string>() == "NavigationCompleted" && item["success"]?.GetValue<bool>() == true));
        holdNavigations = false;
        record["events"] = Since(start);
        return record;
    }

    private async Task<JsonObject> LocaleTwiceCase()
    {
        var record = new JsonObject();
        var start = Now;
        await Open();
        var host = await Frame("");
        await core.ExecuteScriptAsync("(() => { const f = document.createElement('iframe'); f.dataset.scienceFrame = ''; f.src = " + JsonSerializer.Serialize("http://" + host + "/?second") + "; document.getElementById('pane').append(f); return true; })()");
        await Task.Delay(3000);
        lock (gate) record["call_errors"] = callErrors.DeepClone();
        record["benign"] = callErrors.Any(item => ScienceFrame.Benign(item?["error"]?.GetValue<string>() ?? ""));
        await Teardown();
        await Until(6000, () => !policy.Attached);
        record["events"] = Since(start);
        return record;
    }

    private async Task<JsonObject> ProxyCase()
    {
        await LoadWorkbench("/");
        var record = new JsonObject();
        var tree = JsonNode.Parse(await core.CallDevToolsProtocolMethodAsync("Page.getFrameTree", "{}"));
        var frame = tree?["frameTree"]?["frame"]?["id"]?.GetValue<string>();
        var nonce = Guid.NewGuid().ToString("N");
        foreach (var (key, url) in new[] { ("invalid", ScienceFrame.ProxyCheckURL(nonce)), ("loopback", ScienceFrame.ProxyLoopbackURL(nonce)) })
        {
            var load = core.CallDevToolsProtocolMethodAsync("Network.loadNetworkResource", new JsonObject { ["frameId"] = frame, ["url"] = url, ["options"] = ScienceFrame.ProxyCheckOptions() }.ToJsonString());
            if (await Task.WhenAny(load, Task.Delay(ScienceFrame.ProxyCheckMilliseconds)) != load) { record[key] = "timeout"; continue; }
            try { record[key] = JsonNode.Parse(await load)?["resource"]?.DeepClone(); }
            catch (Exception error) { record[key] = "error: " + error.Message; }
        }
        return record;
    }

    // KillSweepCase: a frame stores, the browser process is killed (as a
    // crash or a killed window would end it), a new WebView starts on the same
    // user data folder and sweeps the journal (ScienceFrameHost.Sweep's calls).
    private async Task<JsonObject> KillSweepCase()
    {
        var record = new JsonObject();
        await Open();
        var host = await Frame("store");
        var origin = "http://" + host;
        record["recorded"] = ScienceFrame.Record(data, origin, "http://localhost");
        record["stored"] = await Until(10000, () => Reports(host).Any(report => report["from"]?.GetValue<string>() == "stored"));
        await Task.Delay(2000);
        var browser = core.BrowserProcessId;
        var exited = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        core.Environment.BrowserProcessExited += (_, _) => exited.TrySetResult();
        Process.GetProcessById((int)browser).Kill();
        record["killed"] = (int)browser;
        try { await exited.Task.WaitAsync(TimeSpan.FromSeconds(15)); } catch (TimeoutException) { record["exit_timeout"] = true; }
        Controls.Remove(web);
        web.Dispose();
        web = new WebView2 { Dock = DockStyle.Fill };
        Controls.Add(web);
        await Start();
        await LoadWorkbench("/");
        var swept = new List<string>();
        foreach (var item in ScienceFrame.Journal(data))
        {
            foreach (var call in ScienceFrame.ClearOrigin(item.Origin, item.Site))
                await core.CallDevToolsProtocolMethodAsync(call.Method, call.Params.ToJsonString());
            swept.Add(item.Origin);
        }
        ScienceFrame.Forget(data, swept);
        record["swept"] = new JsonArray(swept.Select(item => (JsonNode)item).ToArray());
        record["journal_left"] = File.Exists(ScienceFrame.JournalPath(data));
        record["left"] = await ProbeStorage(host);
        return record;
    }

    private JsonObject Summary(string host, long teardown)
    {
        var summary = new JsonObject();
        foreach (var group in Reports(host).Where(report => report["from"]?.GetValue<string>() is "doc" or "direct" or "nested").GroupBy(report => report["from"]!.GetValue<string>()))
        {
            JsonObject Values(IEnumerable<JsonObject> items) => new()
            {
                ["count"] = items.Count(),
                ["last_t"] = items.Select(item => item["t"]!.GetValue<long>() - teardown).DefaultIfEmpty(long.MinValue).Max(),
                ["values"] = new JsonArray(items.Select(item => item["lang"] + "|" + item["tz"] + "|" + item["accept_language"] + "|" + item["platform"]).Distinct().Select(text => (JsonNode)text!).ToArray()),
            };
            var first = group.First();
            summary[group.Key] = new JsonObject
            {
                ["first"] = first["lang"] + "|" + first["tz"] + "|" + first["accept_language"] + "|" + first["platform"] + "|" + first["ua"],
                ["before"] = Values(group.Where(item => item["t"]!.GetValue<long>() < teardown)),
                ["after"] = Values(group.Where(item => item["t"]!.GetValue<long>() >= teardown)),
            };
        }
        return summary;
    }

    private JsonArray Since(long since)
    {
        lock (gate) return new JsonArray(events.Where(item => item!["t"]!.GetValue<long>() >= since && item["kind"]?.GetValue<string>() != "request").Select(item => item!.DeepClone()).ToArray());
    }

    private List<JsonObject> Reports(string host)
    {
        lock (gate) return reports.Select(item => item!.AsObject()).Where(item => item["host"]?.GetValue<string>() == host).ToList();
    }

    private async Task<bool> Until(int milliseconds, Func<bool> done)
    {
        var deadline = Now + milliseconds;
        while (Now < deadline)
        {
            if (done()) return true;
            await Task.Delay(50);
        }
        return done();
    }

    private async Task<JsonNode?> Targets()
    {
        try
        {
            var reply = JsonNode.Parse(await core.CallDevToolsProtocolMethodAsync("Target.getTargets", "{}"));
            return new JsonArray((reply?["targetInfos"]?.AsArray() ?? []).Select(info => (JsonNode)(info!["type"] + " " + info["url"])).ToArray());
        }
        catch (Exception error) { return "error: " + error.Message; }
    }

    // Apply follows ScienceFrameHost.Apply: in list order, the front's end
    // (there is no front here: recorded only), crashes (never awaited; their
    // Inspector.targetCrashed is, at most CrashWait), the removal of that
    // origin's frame elements, then the calls per session.
    private async Task Apply(IEnumerable<ScienceFrame.Action> actions, string ending)
    {
        var list = actions.ToList();
        var crashes = new List<Task>();
        foreach (var action in list)
        {
            if (action.End) Event("policy end", new JsonObject());
            else if (action.EndFront) Event("policy endfront", new JsonObject { ["origin"] = ending });
            else if (action.Crash)
            {
                var seen = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                lock (gate) crashWaits[action.Session] = seen;
                var session = action.Session;
                Event("policy crash", new JsonObject { ["session"] = session });
                _ = core.CallDevToolsProtocolMethodForSessionAsync(session, "Page.crash", "{}")
                    .ContinueWith(task => Event("Page.crash reply", new JsonObject { ["session"] = session, ["status"] = task.Status.ToString(), ["error"] = task.Exception?.InnerException?.Message }), TaskScheduler.Default);
                crashes.Add(seen.Task);
            }
            else if (action.Remove)
            {
                await Task.WhenAny(Task.WhenAll(crashes), Task.Delay(ScienceFrame.CrashWaitMilliseconds));
                Event("policy remove", new JsonObject { ["origin"] = ending });
                await core.ExecuteScriptAsync(ScienceFrame.RemoveFrame(ending));
                _ = List();
            }
        }
        if (list.Any(action => action.Session.Length == 0 && action.Call?.Method == "Target.setAutoAttach" && action.Call.Params["autoAttach"]?.GetValue<bool>() == false))
        {
            pageDetachedAt = Now;
            Event("policy page detach", new JsonObject());
        }
        await Task.WhenAll(list.Where(action => action.Call != null).GroupBy(action => action.Session)
            .Select(group => Calls(group.Key, group.Select(action => action.Call!).ToList())));
        if (!list.Any(action => action.Proceed)) return;
        Event("policy proceed", new JsonObject { ["uri"] = heldUri, ["reload"] = heldReload });
        var uri = heldUri;
        heldUri = "";
        if (uri.Length == 0) return;
        if (heldReload) core.Reload(); else core.Navigate(uri);
    }

    private async Task List()
    {
        for (var i = 0; i < 5; i++)
        {
            await Task.Delay(ScienceFrame.ListEveryMilliseconds);
            if (!policy.Attached || !policy.Detaching) return;
            var listed = new List<string>();
            try
            {
                var reply = JsonNode.Parse(await core.CallDevToolsProtocolMethodAsync("Target.getTargets", "{}"));
                listed.AddRange((reply?["targetInfos"]?.AsArray() ?? []).Select(info => info?["targetId"]?.GetValue<string>() ?? ""));
            }
            catch (Exception) { continue; }
            var actions = policy.Listed(listed);
            if (actions.Count != 0) Event("policy listed", new JsonObject { ["actions"] = actions.Count });
            await Apply(actions, "");
        }
    }

    private async Task Calls(string session, List<ScienceFrame.Call> calls)
    {
        foreach (var call in calls)
        {
            try
            {
                if (session.Length == 0) await core.CallDevToolsProtocolMethodAsync(call.Method, call.Params.ToJsonString());
                else await core.CallDevToolsProtocolMethodForSessionAsync(session, call.Method, call.Params.ToJsonString());
            }
            catch (Exception error)
            {
                lock (gate) callErrors.Add(new JsonObject { ["session"] = session, ["method"] = call.Method, ["error"] = error.Message });
                if (ScienceFrame.Benign(error.Message)) continue;
                policy.CallFailed(session);
                await Task.Delay(ScienceFrame.SettleMilliseconds);
                await Apply(policy.Settle(session), "");
                return;
            }
        }
    }

    private void Received(string method, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        var parent = e.SessionId ?? "";
        JsonObject? parameters = null;
        try { parameters = JsonNode.Parse(e.ParameterObjectAsJson) as JsonObject; } catch (JsonException) { }
        var session = parameters?["sessionId"]?.GetValue<string>() ?? "";
        var info = parameters?["targetInfo"] as JsonObject;
        var type = info?["type"]?.GetValue<string>() ?? "";
        Event(method, new JsonObject { ["parent"] = parent, ["session"] = session, ["type"] = type, ["url"] = info?["url"]?.GetValue<string>() });
        switch (method)
        {
            case "Inspector.targetCrashed":
                lock (gate) if (crashWaits.Remove(parent, out var seen)) seen.TrySetResult();
                return;
            case "Target.detachedFromTarget":
                lock (gate) if (crashWaits.Remove(session, out var gone)) gone.TrySetResult();
                _ = Apply(policy.DetachedTarget(session), "");
                return;
        }
        if (parent.Length == 0 && type == "iframe") lock (gate) frameSession = session;
        _ = Apply(policy.AttachedTarget(parent, session, type, info?["targetId"]?.GetValue<string>() ?? ""), "");
    }

    private void Finish()
    {
        lock (gate)
        {
            results["failures"] = failures.DeepClone();
            results["call_errors"] = callErrors.DeepClone();
            results["elapsed_ms"] = Now;
        }
        try { File.WriteAllText(output, results.ToJsonString(new JsonSerializerOptions { WriteIndented = true })); }
        catch (IOException) { }
        workbench?.Dispose();
        labels?.Dispose();
        Close();
    }
}
