using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Web.WebView2.Core;

namespace Cage.Windows;

// ScienceFrameHost drives ScienceFrame.Policy with the window's DevTools
// protocol. It exists only when the WebView was created with
// ScienceFrame.BackstopArguments (some environment has host rendering on);
// otherwise the window keeps every handler it always had and refuses to
// attach.
//
// The guest's identity goes to the Science frame's own targets only: the
// page session auto-attaches (flatten) and every target starts paused; the
// frame's targets get their calls on their own sessions
// (CoreWebView2.CallDevToolsProtocolMethodForSessionAsync; events carry the
// parent's CoreWebView2DevToolsProtocolEventReceivedEventArgs.SessionId).
// The workbench document never gets an override.
internal sealed class ScienceFrameHost
{
    private const string Ended = "页面的一个后台线程或子页面没能使用环境的时区与语言，已停止本机渲染。";
    private const string NavigationDropped = "本机渲染页面没有及时关闭，窗口没有跳转。请再试一次。";
    private const string FrameGone = "本机渲染页面的进程意外退出，已改用隔离浏览器。";
    // POST /_cage/end goes straight to the front's loopback port: never
    // through a proxy.
    private static readonly HttpClient Front = new(new SocketsHttpHandler { UseProxy = false, AllowAutoRedirect = false })
    {
        Timeout = TimeSpan.FromMilliseconds(ScienceFrame.EndWaitMilliseconds),
    };

    private readonly CoreWebView2 core;
    private readonly string data;
    private readonly Action<string> failed;
    private readonly Action<string, bool> navigate;
    private readonly ScienceFrame.Policy policy = new();
    private readonly CoreWebView2DevToolsProtocolEventReceiver attached, detached, crashed;
    private string origin = "";
    private readonly HashSet<string> framed = new(StringComparer.Ordinal);
    private string heldUri = "";
    private bool heldReload;
    private int navigation;
    // Frames this host crashed (their Inspector.targetCrashed is awaited) and
    // the session origins whose frame process exit it caused (N4).
    private readonly Dictionary<string, TaskCompletionSource> crashing = new(StringComparer.Ordinal);
    private readonly HashSet<string> dead = new(StringComparer.Ordinal);
    private readonly Dictionary<string, DateTime> expected = new(StringComparer.Ordinal);

    // data is the WebView's user data folder (the journal of framed
    // origins); failed tells the pane its frame ended; navigate re-issues a
    // navigation this host held (true: as a reload).
    public ScienceFrameHost(CoreWebView2 core, string data, Action<string> failed, Action<string, bool> navigate)
    {
        this.core = core;
        this.data = data;
        this.failed = failed;
        this.navigate = navigate;
        // Frames navigate to the attached session's origin only (and to
        // about:blank or srcdoc documents, which stay in it); the workbench
        // itself has no frames (frame-src 'none' without a front).
        core.FrameNavigationStarting += (_, e) => {
            if (e.Uri is "about:blank" or "about:srcdoc") return;
            if (origin.Length == 0 || !Uri.TryCreate(e.Uri, UriKind.Absolute, out var url) ||
                url.GetLeftPart(UriPartial.Authority) != origin || url.UserInfo.Length != 0) e.Cancel = true;
        };
        attached = core.GetDevToolsProtocolEventReceiver("Target.attachedToTarget");
        attached.DevToolsProtocolEventReceived += Attached;
        detached = core.GetDevToolsProtocolEventReceiver("Target.detachedFromTarget");
        detached.DevToolsProtocolEventReceived += Detached;
        crashed = core.GetDevToolsProtocolEventReceiver("Inspector.targetCrashed");
        crashed.DevToolsProtocolEventReceived += (_, e) => {
            var session = e.SessionId ?? "";
            dead.Add(session);
            if (crashing.Remove(session, out var seen)) seen.TrySetResult();
        };
    }

    // Prepare drops any earlier identity and proves the proxy switch (F5).
    public async Task<object> Prepare()
    {
        await Detach();
        return new { backstop = true, proxy = await ProxyCheck() };
    }

    // Attach returns once the page session's own calls are acknowledged (the
    // pane frames the session only then); a failure throws, and the pane
    // falls back to the browser pane.
    public async Task Attach(string url)
    {
        var next = ScienceFrame.Identity(Backend.Root, url) ?? throw new InvalidDataException("找不到这个本机渲染页面对应的环境，请回到 Claude Science 重新打开。");
        var label = ScienceFrame.LabelOrigin(url)!;
        // Before the frame can store anything: a start after a kill or a
        // crash clears what it stored (N6).
        if (!Uri.TryCreate(core.Source, UriKind.Absolute, out var source) || !ScienceFrame.Record(data, label, source.Scheme + "://" + source.Host))
            throw new InvalidDataException("窗口没能记录本机渲染页面的存储位置，本机渲染未打开。");
        var actions = policy.Arm(next);
        if (actions.Any(action => action.End)) throw new InvalidDataException("窗口正在跳转，本机渲染未打开。");
        origin = label;
        framed.Add(origin);
        var page = actions.Where(action => action.Session.Length == 0 && action.Call != null).ToList();
        try
        {
            foreach (var action in page) await Send("", action.Call!);
        }
        catch (Exception error) when (error is InvalidOperationException or COMException or ArgumentException)
        {
            throw new InvalidDataException("窗口没能为本机渲染页面准备时区与语言，本机渲染未打开。", error);
        }
        finally
        {
            _ = Apply(actions.Where(action => !(action.Session.Length == 0 && action.Call != null)), "");
        }
    }

    // Detach is the pane's detach op: it returns once the frame's teardown
    // (the front's end, the crash, the removal) has run; the targets'
    // detaches and the end of auto-attach follow.
    public Task Detach()
    {
        var ending = origin;
        origin = "";
        return Apply(policy.Detach(), ending);
    }

    // Hold answers NavigationStarting for the window's own document: true
    // cancels it, and it is issued again once the frame's targets are gone
    // (N7): a reload as a reload; anything else as a navigation to its
    // address, so a back or forward one becomes a new history entry and a
    // form's POST body would be lost (the workbench submits no forms).
    public bool Hold(string uri, bool reload)
    {
        var ending = origin;
        origin = "";
        var actions = policy.Navigate();
        if (actions.Any(action => action.Proceed))
        {
            _ = Apply(actions.Where(action => !action.Proceed), ending);
            return false;
        }
        heldUri = uri;
        heldReload = reload;
        var current = ++navigation;
        _ = Apply(actions, ending);
        _ = DropAfter(current);
        return true;
    }

    // FrameExited answers ProcessFailed of kind FrameRenderProcessExited
    // (N4): a frame's process is gone, the window's document is not. The
    // exit of a frame this host crashed says nothing; any other exit of the
    // Science frame ends its front and tells the pane, which falls back to
    // the browser pane. It never fails the window.
    public void FrameExited(IReadOnlyList<CoreWebView2FrameInfo>? frames)
    {
        if (origin.Length == 0) return;
        var now = DateTime.UtcNow;
        foreach (var stale in expected.Where(pair => now - pair.Value > TimeSpan.FromSeconds(10)).Select(pair => pair.Key).ToList()) expected.Remove(stale);
        var origins = (frames ?? []).Select(frame => Uri.TryCreate(frame.Source, UriKind.Absolute, out var url) ? url.GetLeftPart(UriPartial.Authority) : "").ToList();
        if (origins.Count != 0 ? origins.All(expected.ContainsKey) : expected.Count != 0) return;
        if (origins.Count != 0 && !origins.Contains(origin)) return;
        _ = EndFront(origin);
        failed(FrameGone);
    }

    // EndForExit ends the front's session before the WebView goes (the
    // window quits or recreates it, or its document's process is gone): its
    // processes end asynchronously and a worker must not reach the daemon
    // without its overrides meanwhile.
    public void EndForExit()
    {
        var ending = origin;
        origin = "";
        try { EndFront(ending).Wait(ScienceFrame.EndWaitMilliseconds); }
        catch (AggregateException) { }
    }

    private async Task DropAfter(int current)
    {
        await Task.Delay(ScienceFrame.NavigateWaitMilliseconds);
        if (current != navigation) return;
        if (policy.NavigateTimedOut().Count == 0) return;
        heldUri = "";
        failed(NavigationDropped);
    }

    // Apply performs actions in list order: the front's end for the frame of
    // origin ending, crashes (Page.crash never answers while the frame
    // lives: its Inspector.targetCrashed is awaited, at most CrashWait), the
    // removal of that origin's frame elements, then the calls, in order per
    // session.
    private async Task Apply(IEnumerable<ScienceFrame.Action> actions, string ending)
    {
        var list = actions.ToList();
        // The origins to clear are the ones framed when the page detach was
        // produced: an attach that follows adds its own for the next one.
        List<string>? clear = null;
        if (list.Any(action => action.Session.Length == 0 && action.Call?.Method == "Target.setAutoAttach" && action.Call.Params["autoAttach"]?.GetValue<bool>() == false))
        {
            clear = framed.ToList();
            framed.Clear();
        }
        var crashes = new List<Task>();
        foreach (var action in list)
        {
            if (action.End) failed(Ended);
            else if (action.EndFront) await EndFront(ending);
            else if (action.Crash) crashes.Add(Crash(action.Session, ending));
            else if (action.Remove)
            {
                await Task.WhenAny(Task.WhenAll(crashes), Task.Delay(ScienceFrame.CrashWaitMilliseconds));
                await Script(ScienceFrame.RemoveFrame(ending));
                _ = List();
            }
        }
        await Task.WhenAll(list.Where(action => action.Call != null).GroupBy(action => action.Session)
            .Select(group => Run(group.Key, group.Select(action => action.Call!).ToList())));
        if (clear != null) await ClearFramed(clear);
        if (!list.Any(action => action.Proceed)) return;
        navigation++;
        var uri = heldUri;
        heldUri = "";
        if (uri.Length != 0) navigate(uri, heldReload);
    }

    // EndFront asks the front of a session origin to end that frame's
    // session (POST /_cage/end) and waits for its answer, at most EndWait. A
    // failure is left to the crash and the removal that follow.
    private static async Task EndFront(string ending)
    {
        var end = ending.Length == 0 ? null : ScienceFrame.End(Backend.Root, ending);
        if (end == null) return;
        using var request = new HttpRequestMessage(HttpMethod.Post, end.Address);
        request.Headers.Add("X-Cage-End", end.Secret);
        request.Headers.Add("X-Cage-End-Label", end.Label);
        try { using var _ = await Front.SendAsync(request).ConfigureAwait(false); }
        catch (Exception error) when (error is HttpRequestException or TaskCanceledException or InvalidOperationException) { }
    }

    // Crash sends Page.crash to a frame's session without awaiting it (it
    // never answers) and returns once the frame's Inspector.targetCrashed
    // arrived. The frame's process exit is expected (FrameExited).
    private Task Crash(string session, string ending)
    {
        if (dead.Contains(session)) return Task.CompletedTask;
        var seen = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        crashing[session] = seen;
        if (ending.Length != 0) expected[ending] = DateTime.UtcNow;
        try
        {
            _ = core.CallDevToolsProtocolMethodForSessionAsync(session, "Page.crash", "{}")
                .ContinueWith(task => _ = task.Exception, TaskScheduler.Default);
        }
        catch (Exception error) when (error is InvalidOperationException or COMException or ArgumentException) { seen.TrySetResult(); }
        return seen.Task;
    }

    // List reconciles while the frame goes: the page session's
    // Target.getTargets every ListEvery until auto-attach has ended (at most
    // five times).
    private async Task List()
    {
        for (var i = 0; i < 5; i++)
        {
            await Task.Delay(ScienceFrame.ListEveryMilliseconds);
            if (!policy.Attached || !policy.Detaching) return;
            var listed = new List<string>();
            try
            {
                using var reply = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Target.getTargets", "{}"));
                foreach (var info in reply.RootElement.GetProperty("targetInfos").EnumerateArray())
                    listed.Add(info.GetProperty("targetId").GetString() ?? "");
            }
            catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException or COMException or ArgumentException) { continue; }
            await Apply(policy.Listed(listed), "");
        }
    }

    // Run sends one session's calls in order. A failed call never resumes
    // its target: the policy settles it after SettleMilliseconds.
    private async Task Run(string session, List<ScienceFrame.Call> calls)
    {
        foreach (var call in calls)
        {
            try { await Send(session, call); }
            catch (Exception)
            {
                policy.CallFailed(session);
                await Task.Delay(ScienceFrame.SettleMilliseconds);
                await Apply(policy.Settle(session), "");
                return;
            }
        }
    }

    // ClearFramed empties the storage of session origins whose targets are
    // gone (framed before the page detach) and forgets them in the journal.
    private async Task ClearFramed(List<string> origins)
    {
        var journal = ScienceFrame.Journal(data).ToDictionary(item => item.Origin, item => item.Site, StringComparer.Ordinal);
        var site = Uri.TryCreate(core.Source, UriKind.Absolute, out var page) ? page.Scheme + "://" + page.Host : "";
        var cleared = new List<string>();
        foreach (var framedOrigin in origins)
            if (await Clear(core, framedOrigin, journal.GetValueOrDefault(framedOrigin, site))) cleared.Add(framedOrigin);
        ScienceFrame.Forget(data, cleared);
    }

    private static async Task<bool> Clear(CoreWebView2 core, string framedOrigin, string site)
    {
        if (site.Length == 0) return false;
        foreach (var call in ScienceFrame.ClearOrigin(framedOrigin, site))
        {
            try { await core.CallDevToolsProtocolMethodAsync(call.Method, call.Params.ToJsonString()); }
            catch (Exception error) when (error is InvalidOperationException or COMException or ArgumentException) { return false; }
        }
        return true;
    }

    // Sweep runs at every start of the WebView, with or without the
    // switches: the origins a killed or crashed window framed and never
    // cleared are cleared now. Without a journal it is one existence check.
    public static async Task Sweep(CoreWebView2 core, string data)
    {
        var framed = ScienceFrame.Journal(data);
        if (framed.Count == 0) return;
        var cleared = new List<string>();
        foreach (var item in framed)
            if (await Clear(core, item.Origin, item.Site)) cleared.Add(item.Origin);
        ScienceFrame.Forget(data, cleared);
    }

    private async Task Send(string session, ScienceFrame.Call call)
    {
        var parameters = call.Params.ToJsonString();
        try
        {
            if (session.Length == 0) await core.CallDevToolsProtocolMethodAsync(call.Method, parameters);
            else await core.CallDevToolsProtocolMethodForSessionAsync(session, call.Method, parameters);
        }
        catch (Exception error) when (ScienceFrame.Benign(error.Message)) { }
    }

    private async Task Script(string script)
    {
        try { await core.ExecuteScriptAsync(script); }
        catch (Exception error) when (error is InvalidOperationException or COMException) { }
    }

    private static bool Read(string json, out string session, out string type, out string target)
    {
        session = type = target = "";
        try
        {
            using var data = JsonDocument.Parse(json);
            session = data.RootElement.GetProperty("sessionId").GetString() ?? "";
            if (data.RootElement.TryGetProperty("targetInfo", out var info))
            {
                type = info.GetProperty("type").GetString() ?? "";
                target = info.GetProperty("targetId").GetString() ?? "";
            }
            return session.Length != 0;
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException) { return false; }
    }

    private void Attached(object? sender, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        if (Read(e.ParameterObjectAsJson, out var session, out var type, out var target)) _ = Apply(policy.AttachedTarget(e.SessionId ?? "", session, type, target), "");
    }

    private void Detached(object? sender, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        if (!Read(e.ParameterObjectAsJson, out var session, out _, out _)) return;
        dead.Remove(session);
        if (crashing.Remove(session, out var seen)) seen.TrySetResult();
        _ = Apply(policy.DetachedTarget(session), "");
    }

    // ProxyCheck loads ScienceFrame.ProxyCheckURL, then ProxyLoopbackURL,
    // under the window's main frame (the workbench document allows both in
    // connect-src while a front runs): "verified" only when the switch's
    // proxy refused both. Each load gets ProxyCheckMilliseconds (a direct
    // connection to 127.0.0.3 can hang where nothing refuses it).
    private async Task<string> ProxyCheck()
    {
        try
        {
            using var tree = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Page.getFrameTree", "{}"));
            var frame = tree.RootElement.GetProperty("frameTree").GetProperty("frame").GetProperty("id").GetString();
            var nonce = Guid.NewGuid().ToString("N");
            foreach (var url in new[] { ScienceFrame.ProxyCheckURL(nonce), ScienceFrame.ProxyLoopbackURL(nonce) })
            {
                var parameters = new JsonObject {
                    ["frameId"] = frame,
                    ["url"] = url,
                    ["options"] = ScienceFrame.ProxyCheckOptions(),
                };
                var load = core.CallDevToolsProtocolMethodAsync("Network.loadNetworkResource", parameters.ToJsonString());
                if (await Task.WhenAny(load, Task.Delay(ScienceFrame.ProxyCheckMilliseconds)) != load)
                {
                    _ = load.ContinueWith(task => _ = task.Exception, TaskScheduler.Default);
                    return "failed";
                }
                using var reply = JsonDocument.Parse(await load);
                var resource = reply.RootElement.GetProperty("resource");
                var name = resource.TryGetProperty("netErrorName", out var error) ? error.GetString() ?? "" : "";
                if (ScienceFrame.ProxyVerdict(resource.GetProperty("success").GetBoolean(), name) != "verified") return "failed";
            }
            return "verified";
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException or COMException or ArgumentException)
        {
            return "failed";
        }
    }
}
