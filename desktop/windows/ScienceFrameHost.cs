using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Web.WebView2.Core;

namespace Cage.Windows;

// ScienceFrameHost applies ScienceFrame's calls to the window's WebView. It
// exists only when the WebView was created with ScienceFrame.BackstopArguments
// (some environment has host rendering on); otherwise the window keeps every
// handler it always had and refuses to arm.
//
// The overrides are WebView-wide (the workbench page sees the guest's time
// zone and language while the frame shows); the pane disarms when the frame
// goes away, and this host disarms on a main-frame navigation or a renderer
// failure.
internal sealed class ScienceFrameHost
{
    private readonly CoreWebView2 core;
    private readonly Action<string> failed;
    private readonly CoreWebView2DevToolsProtocolEventReceiver attached;
    private JsonObject? identity;
    private string origin = "";
    private int generation;

    public ScienceFrameHost(CoreWebView2 core, Action<string> failed)
    {
        this.core = core;
        this.failed = failed;
        // Frames navigate to the armed front only; the workbench itself
        // has no frames (its CSP is frame-src 'none' without a front).
        core.FrameNavigationStarting += (_, e) => {
            if (origin.Length == 0 || !Uri.TryCreate(e.Uri, UriKind.Absolute, out var url) ||
                url.GetLeftPart(UriPartial.Authority) != origin || url.UserInfo.Length != 0) e.Cancel = true;
        };
        attached = core.GetDevToolsProtocolEventReceiver("Target.attachedToTarget");
        attached.DevToolsProtocolEventReceived += Attached;
    }

    public bool Armed => identity != null;

    public async Task Arm(string url)
    {
        var next = ScienceFrame.Identity(Backend.Root, url) ?? throw new InvalidDataException("找不到这个本机渲染页面对应的环境，请回到 Claude Science 重新打开。");
        await Disarm();
        var current = ++generation;
        try
        {
            foreach (var call in ScienceFrame.Arm(next)) await Send("", call);
        }
        catch
        {
            await Disarm();
            throw new InvalidDataException("无法把环境的时区、语言和浏览器标识交给窗口，已停止本机渲染。");
        }
        if (current != generation)
        {
            // Disarmed while arming: undo whatever reached the page after it.
            await Disarm();
            throw new InvalidDataException("本机渲染已停止。");
        }
        identity = next;
        origin = new Uri(url).GetLeftPart(UriPartial.Authority);
    }

    public async Task Disarm()
    {
        generation++;
        identity = null;
        origin = "";
        foreach (var call in ScienceFrame.Disarm)
        {
            try { await Send("", call); }
            catch (Exception error) when (error is InvalidOperationException or System.Runtime.InteropServices.COMException or ArgumentException) { }
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

    // Every new worker and frame starts paused (waitForDebuggerOnStart):
    // it gets the identity, then runs. It always runs, so a failure cannot
    // hang the page; a worker or frame that could not take the identity
    // ends the frame instead.
    private async void Attached(object? sender, CoreWebView2DevToolsProtocolEventReceivedEventArgs e)
    {
        string session = "", type = "";
        try
        {
            using var data = JsonDocument.Parse(e.ParameterObjectAsJson);
            session = data.RootElement.GetProperty("sessionId").GetString() ?? "";
            type = data.RootElement.GetProperty("targetInfo").GetProperty("type").GetString() ?? "";
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException) { return; }
        if (session.Length == 0) return;
        var current = identity;
        var calls = current == null ? [ScienceFrame.Resume] : ScienceFrame.ForTarget(type, current);
        try
        {
            foreach (var call in calls) await Send(session, call);
        }
        catch (Exception)
        {
            try { await Send(session, ScienceFrame.Resume); }
            catch (Exception) { }
            if (current != null && type is "worker" or "iframe")
            {
                await Disarm();
                failed("页面的一个后台线程没能使用环境的时区与语言，已停止本机渲染。");
            }
        }
    }
}
