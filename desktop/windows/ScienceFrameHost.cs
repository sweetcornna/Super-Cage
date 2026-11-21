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
    // The pane marks its frame element (claude-science.jsx).
    private const string RemoveFrame = "document.querySelectorAll('iframe[data-science-frame]').forEach(frame => frame.remove());";

    private readonly CoreWebView2 core;
    private readonly Action<string> failed;
    private readonly Action<string> navigate;
    private readonly ScienceFrame.Policy policy = new();
    private readonly CoreWebView2DevToolsProtocolEventReceiver attached, detached;
    private string origin = "";
    private readonly HashSet<string> framed = new(StringComparer.Ordinal);
    private string heldUri = "";
    private int navigation;

    // failed tells the pane its frame ended; navigate re-issues a navigation
    // this host held.
    public ScienceFrameHost(CoreWebView2 core, Action<string> failed, Action<string> navigate)
    {
        this.core = core;
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
    }

    // Prepare drops any earlier identity and proves the proxy switch (F5).
    public async Task<object> Prepare()
    {
        origin = "";
        Apply(policy.Detach());
        return new { backstop = true, proxy = await ProxyCheck() };
    }

    // Attach returns once the page session's own calls are acknowledged (the
    // pane frames the session only then); a failure throws, and the pane
    // falls back to the browser pane.
    public async Task Attach(string url)
    {
        var next = ScienceFrame.Identity(Backend.Root, url) ?? throw new InvalidDataException("找不到这个本机渲染页面对应的环境，请回到 Claude Science 重新打开。");
        var actions = policy.Arm(next);
        if (actions.Any(action => action.End)) throw new InvalidDataException("窗口正在跳转，本机渲染未打开。");
        origin = ScienceFrame.LabelOrigin(url)!;
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
            Apply(actions.Where(action => !(action.Session.Length == 0 && action.Call != null)));
        }
    }

    public void Detach()
    {
        origin = "";
        Apply(policy.Detach());
    }

    // Hold answers NavigationStarting for the window's own document: true
    // cancels it, and it is issued again once the frame's targets are gone.
    public bool Hold(string uri)
    {
        origin = "";
        var actions = policy.Navigate();
        if (actions.Any(action => action.Proceed))
        {
            Apply(actions.Where(action => !action.Proceed));
            return false;
        }
        heldUri = uri;
        var current = ++navigation;
        Apply(actions);
        _ = DropAfter(current);
        return true;
    }

    private async Task DropAfter(int current)
    {
        await Task.Delay(ScienceFrame.NavigateWaitMilliseconds);
        if (current != navigation) return;
        if (policy.NavigateTimedOut().Count == 0) return;
        heldUri = "";
        failed(NavigationDropped);
    }

    private async void Apply(IEnumerable<ScienceFrame.Action> actions)
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
        foreach (var action in list)
        {
            if (action.End) failed(Ended);
            else if (action.Remove) await Script(RemoveFrame);
        }
        await Task.WhenAll(list.Where(action => action.Call != null).GroupBy(action => action.Session)
            .Select(group => Run(group.Key, group.Select(action => action.Call!).ToList())));
        if (clear != null) await ClearFramed(clear);
        if (!list.Any(action => action.Proceed)) return;
        navigation++;
        var uri = heldUri;
        heldUri = "";
        if (uri.Length != 0) navigate(uri);
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
                Apply(policy.Settle(session));
                return;
            }
        }
    }

    // ClearFramed empties the storage of session origins whose targets are
    // gone (framed before the page detach).
    private async Task ClearFramed(List<string> origins)
    {
        if (!Uri.TryCreate(core.Source, UriKind.Absolute, out var page)) return;
        var site = page.Scheme + "://" + page.Host;
        foreach (var framedOrigin in origins)
        {
            foreach (var call in ScienceFrame.ClearOrigin(framedOrigin, site))
            {
                try { await Send("", call); }
                catch (Exception error) when (error is InvalidOperationException or COMException or ArgumentException) { }
            }
        }
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

    private static bool Read(string json, out string session, out string type)
    {
        session = type = "";
        try
        {
            using var data = JsonDocument.Parse(json);
            session = data.RootElement.GetProperty("sessionId").GetString() ?? "";
            if (data.RootElement.TryGetProperty("targetInfo", out var info)) type = info.GetProperty("type").GetString() ?? "";
            return session.Length != 0;
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException) { return false; }
    }

    private void Attached(object? sender, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        if (Read(e.ParameterObjectAsJson, out var session, out var type)) Apply(policy.AttachedTarget(e.SessionId ?? "", session, type));
    }

    private void Detached(object? sender, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        if (Read(e.ParameterObjectAsJson, out var session, out _)) Apply(policy.DetachedTarget(session));
    }

    // ProxyCheck loads ScienceFrame.ProxyCheckURL under the window's main
    // frame (the workbench document allows it in connect-src while a front
    // runs): "verified" only when the switch's proxy refused the connection.
    private async Task<string> ProxyCheck()
    {
        try
        {
            using var tree = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Page.getFrameTree", "{}"));
            var frame = tree.RootElement.GetProperty("frameTree").GetProperty("frame").GetProperty("id").GetString();
            var parameters = new JsonObject {
                ["frameId"] = frame,
                ["url"] = ScienceFrame.ProxyCheckURL(Guid.NewGuid().ToString("N")),
                ["options"] = ScienceFrame.ProxyCheckOptions(),
            };
            using var reply = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Network.loadNetworkResource", parameters.ToJsonString()));
            var resource = reply.RootElement.GetProperty("resource");
            var name = resource.TryGetProperty("netErrorName", out var error) ? error.GetString() ?? "" : "";
            return ScienceFrame.ProxyVerdict(resource.GetProperty("success").GetBoolean(), name);
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException or COMException or ArgumentException)
        {
            return "failed";
        }
    }
}
