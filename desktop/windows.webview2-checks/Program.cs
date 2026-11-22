using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace Cage.Windows;

// Cage.WebView2Checks <results.json> <user data folder>: measures, in the
// installed WebView2 runtime, what Claude Science host rendering assumes
// (README.md lists the checks). It opens one small window, runs every case,
// writes the results and closes the window.
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
    private readonly WebView2 web = new() { Dock = DockStyle.Fill };
    private readonly Stopwatch clock = Stopwatch.StartNew();
    private readonly object gate = new();
    private readonly JsonArray events = [], reports = [], failures = [];
    private readonly JsonObject results = new();
    private readonly HashSet<string> guests = new(StringComparer.Ordinal);
    private readonly Dictionary<string, TaskCompletionSource> crashed = new(StringComparer.Ordinal);
    private string frameSession = "";
    private CoreWebView2 core = null!;
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
        ShowInTaskbar = true;
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

    private const string Workbench = """
        <!doctype html><meta charset="utf-8"><title>workbench</title><div id="pane"></div>
        <script>
        fetch("/report", { method: "POST", body: JSON.stringify({ from: "workbench", lang: navigator.language, langs: navigator.languages, tz: Intl.DateTimeFormat().resolvedOptions().timeZone, ua: navigator.userAgent, platform: navigator.platform }) });
        </script>
        """;

    private const string Reporter = """
        const report = (from) => fetch("/report", { method: "POST", body: JSON.stringify({ from, lang: navigator.language, langs: navigator.languages, tz: Intl.DateTimeFormat().resolvedOptions().timeZone, ua: navigator.userAgent, platform: navigator.platform }) });
        const loop = async (from) => { for (;;) { try { await report(from); } catch {} await new Promise((resolve) => setTimeout(resolve, 25)); } };
        """;

    private static string Label(bool busy) => "<!doctype html><meta charset=\"utf-8\"><title>label</title><script>" + Reporter + """
        loop("doc");
        new Worker("/direct.js");
        """ + (busy ? "addEventListener('pagehide', () => { const t = Date.now(); while (Date.now() - t < 3000) {} });" : "") + "</script>";

    private LoopbackServer.Response Serve(LoopbackServer.Request request, bool label)
    {
        var path = request.Path.Split('?')[0];
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
        if (!label && path == "/") return new(200, "text/html; charset=utf-8", Workbench);
        if (label && path == "/") return new(200, "text/html; charset=utf-8", Label(request.Path.Contains("busy")));
        if (label && path == "/direct.js") return new(200, "text/javascript", Reporter + "new Worker('/nested.js'); loop('direct');");
        if (label && path == "/nested.js") return new(200, "text/javascript", Reporter + "loop('nested');");
        return new(404, "text/plain", "not found");
    }

    private async Task Run()
    {
        using var watchdog = new System.Windows.Forms.Timer { Interval = 240000 };
        watchdog.Tick += (_, _) => { results["watchdog"] = true; Finish(); };
        watchdog.Start();
        try
        {
            results["runtime_available"] = CoreWebView2Environment.GetAvailableBrowserVersionString();
            results["arguments"] = ScienceFrame.BackstopArguments;
            var environment = await CoreWebView2Environment.CreateAsync(null, data, new CoreWebView2EnvironmentOptions(ScienceFrame.BackstopArguments));
            await web.EnsureCoreWebView2Async(environment);
            core = web.CoreWebView2;
            results["runtime"] = environment.BrowserVersionString;
            core.ProcessFailed += (_, e) =>
            {
                var frames = new JsonArray();
                try { foreach (var frame in e.FrameInfosForFailedProcess ?? []) frames.Add(new JsonObject { ["name"] = frame.Name, ["source"] = frame.Source }); }
                catch (Exception error) { frames.Add(error.GetType().Name); }
                Event("ProcessFailed", new JsonObject { ["process_kind"] = e.ProcessFailedKind.ToString(), ["reason"] = e.Reason.ToString(), ["exit_code"] = e.ExitCode, ["description"] = e.ProcessDescription, ["frames"] = frames });
            };
            foreach (var method in new[] { "Target.attachedToTarget", "Target.detachedFromTarget", "Inspector.targetCrashed" })
                core.GetDevToolsProtocolEventReceiver(method).DevToolsProtocolEventReceived += (_, e) => Received(method, e);
            using var workbench = new LoopbackServer(request => Serve(request, false));
            using var labels = new LoopbackServer(request => Serve(request, true));
            var cases = new JsonObject();
            foreach (var name in new[] { "remove", "remove-busy", "crash", "move", "hung" })
                cases[name] = await Case(name, workbench.Port, labels.Port);
            results["cases"] = cases;
            ExitCode = 0;
        }
        catch (Exception error)
        {
            results["error"] = error.ToString();
        }
        Finish();
    }

    private async Task<JsonObject> Case(string name, int workbenchPort, int labelPort)
    {
        var record = new JsonObject();
        var navigated = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        void Completed(object? sender, CoreWebView2NavigationCompletedEventArgs e) => navigated.TrySetResult(e.IsSuccess);
        core.NavigationCompleted += Completed;
        core.Navigate("http://localhost:" + workbenchPort + "/?case=" + name);
        record["navigated"] = await navigated.Task.WaitAsync(TimeSpan.FromSeconds(15));
        core.NavigationCompleted -= Completed;
        lock (gate) { guests.Clear(); frameSession = ""; }
        await Page("Target.setAutoAttach", new JsonObject { ["autoAttach"] = true, ["waitForDebuggerOnStart"] = true, ["flatten"] = true });
        var label = Guid.NewGuid().ToString("N");
        var host = label + ".localhost:" + labelPort;
        var url = "http://" + host + "/" + (name == "remove-busy" ? "?busy" : "");
        var start = Now;
        record["start"] = start;
        record["host"] = host;
        await core.ExecuteScriptAsync("(() => { const f = document.createElement('iframe'); f.id = 'science'; f.dataset.scienceFrame = ''; f.setAttribute('sandbox', 'allow-scripts allow-same-origin allow-forms'); f.style.cssText = 'width:300px;height:120px;border:0'; f.src = " + JsonSerializer.Serialize(url) + "; document.getElementById('pane').append(f); return true; })()");
        var ready = await Until(10000, () => new[] { "doc", "direct", "nested" }.All(from => Reports(host).Any(report => report["from"]?.GetValue<string>() == from)));
        record["all_reporting"] = ready;
        await Task.Delay(500);
        record["targets_before"] = await Targets();
        string frame;
        lock (gate) frame = frameSession;
        record["frame_session"] = frame;
        var teardown = Now;
        record["teardown"] = teardown;
        switch (name)
        {
            case "remove":
            case "remove-busy":
                await Remove();
                await Task.Delay(7000);
                break;
            case "crash":
                record["crash"] = await Crash(frame);
                await Task.Delay(1000);
                record["removed_at"] = Now;
                await Remove();
                await Task.Delay(3000);
                break;
            case "move":
                record["moved"] = (await core.ExecuteScriptAsync("(() => { const f = document.getElementById('science'); if (!document.body.moveBefore) return 'no moveBefore'; f.hidden = true; document.body.moveBefore(f, null); document.getElementById('pane').remove(); return 'moved ' + f.isConnected; })()"));
                await Task.Delay(1500);
                record["crash_at"] = Now;
                record["crash"] = await Crash(frame);
                record["removed_at"] = Now;
                await Remove();
                await Task.Delay(3000);
                break;
            case "hung":
                _ = Session(frame, "Runtime.evaluate", new JsonObject { ["expression"] = "while (true) {}" });
                await Task.Delay(500);
                record["crash_at"] = Now;
                record["crash"] = await Crash(frame);
                record["removed_at"] = Now;
                await Remove();
                await Task.Delay(3000);
                break;
        }
        record["targets_after"] = await Targets();
        lock (gate) record["guests_left"] = new JsonArray(guests.Select(session => (JsonNode)session).ToArray());
        await Page("Target.setAutoAttach", new JsonObject { ["autoAttach"] = false, ["waitForDebuggerOnStart"] = false, ["flatten"] = true });
        // Per reporter: what it reported before the teardown and after it.
        var summary = new JsonObject();
        foreach (var group in Reports(host).GroupBy(report => report["from"]?.GetValue<string>() ?? ""))
        {
            JsonObject Values(IEnumerable<JsonObject> items) => new()
            {
                ["count"] = items.Count(),
                ["last_t"] = items.Select(item => item["t"]!.GetValue<long>()).DefaultIfEmpty(-1).Max(),
                ["values"] = new JsonArray(items.Select(item => item["lang"] + "|" + item["tz"] + "|" + item["accept_language"] + "|" + item["platform"]).Distinct().Select(text => (JsonNode)text!).ToArray()),
            };
            summary[group.Key] = new JsonObject
            {
                ["before"] = Values(group.Where(item => item["t"]!.GetValue<long>() < teardown)),
                ["after"] = Values(group.Where(item => item["t"]!.GetValue<long>() >= teardown)),
            };
        }
        record["reports"] = summary;
        lock (gate) record["events"] = new JsonArray(events.Where(item => item!["t"]!.GetValue<long>() >= start).Select(item => item!.DeepClone()).ToArray());
        lock (gate) record["workbench"] = new JsonArray(reports.Where(item => item!["from"]?.GetValue<string>() == "workbench" && item["t"]!.GetValue<long>() >= start - 3000).Select(item => item!.DeepClone()).Take(1).ToArray());
        return record;
    }

    private IEnumerable<JsonObject> Reports(string host)
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

    private Task Remove() => core.ExecuteScriptAsync("document.querySelectorAll('iframe[data-science-frame]').forEach(frame => frame.remove()); true");

    // Crash fires Page.crash on the frame's session (it never answers while
    // the frame lives) and waits up to 3 s for Inspector.targetCrashed.
    private async Task<JsonObject> Crash(string session)
    {
        var record = new JsonObject { ["sent"] = Now };
        var seen = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        lock (gate) crashed[session] = seen;
        var call = core.CallDevToolsProtocolMethodForSessionAsync(session, "Page.crash", "{}");
        _ = call.ContinueWith(task => Event("Page.crash reply", new JsonObject
        {
            ["session"] = session,
            ["status"] = task.Status.ToString(),
            ["error"] = task.Exception?.InnerException?.Message,
        }), TaskScheduler.Default);
        try { await seen.Task.WaitAsync(TimeSpan.FromSeconds(3)); record["target_crashed"] = Now; }
        catch (TimeoutException) { record["target_crashed"] = null; }
        return record;
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

    private async Task Page(string method, JsonObject parameters)
    {
        try { await core.CallDevToolsProtocolMethodAsync(method, parameters.ToJsonString()); }
        catch (Exception error) { lock (gate) failures.Add("page " + method + ": " + error.Message); }
    }

    private async Task Session(string session, string method, JsonObject parameters)
    {
        try { await core.CallDevToolsProtocolMethodForSessionAsync(session, method, parameters.ToJsonString()); }
        catch (Exception error) when (!ScienceFrame.Benign(error.Message)) { lock (gate) failures.Add(session + " " + method + ": " + error.Message); }
        catch (Exception) { }
    }

    private async Task Calls(string session, IEnumerable<ScienceFrame.Call> calls)
    {
        foreach (var call in calls) await Session(session, call.Method, call.Params);
    }

    // Received mirrors ScienceFrame.Policy closely enough for the checks: the
    // page-level iframe and everything under it get the identity; anything
    // else runs at once.
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
                lock (gate) if (crashed.Remove(parent, out var seen)) seen.TrySetResult();
                return;
            case "Target.detachedFromTarget":
                lock (gate) guests.Remove(session);
                return;
        }
        bool guest;
        lock (gate)
        {
            guest = parent.Length == 0 ? type == "iframe" : guests.Contains(parent) && type is "worker" or "iframe";
            if (guest) guests.Add(session);
            if (guest && parent.Length == 0) frameSession = session;
        }
        _ = guest ? Calls(session, type == "worker" ? ScienceFrame.Worker(Identity) : ScienceFrame.Frame(Identity)) : Calls(session, ScienceFrame.Resume);
    }

    private void Finish()
    {
        lock (gate)
        {
            results["failures"] = failures.DeepClone();
            results["elapsed_ms"] = Now;
        }
        try { File.WriteAllText(output, results.ToJsonString(new JsonSerializerOptions { WriteIndented = true })); }
        catch (IOException) { }
        Close();
    }
}
