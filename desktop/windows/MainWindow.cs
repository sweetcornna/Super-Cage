using System.Text.Json;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace Cage.Windows;

internal sealed class MainWindow : Form
{
    private WebView2 web = new() { Dock = DockStyle.Fill, Visible = false };
    private readonly NativeInput nativeInput;
    private readonly Label message = new() { AutoSize = true, MaximumSize = new Size(720, 0), Text = "正在启动 Super Cage 工作台…", Margin = new Padding(12) };
    private readonly FlowLayoutPanel status = new() { Dock = DockStyle.Fill, FlowDirection = FlowDirection.TopDown, Padding = new Padding(48), WrapContents = false };
    private readonly Button retry = new() { Text = "重试连接", AutoSize = true, Visible = false };
    private readonly Button existing = new() { Text = "打开现有环境保存文件", AutoSize = true, Visible = false };
    private readonly Button complete = new() { Text = "保存后停止环境并完成更新", AutoSize = true, Visible = false };
    private readonly NotifyIcon tray;
    private readonly AppUpdater updater;
    private int expectedPort;
    private bool controllerNeedsUpdate, allowQuit, bootstrapping, webFailed;
    private bool completingSmoke;
    private string smokePhase = "starting";
    // Smoke only: what the Claude Science part of a smoke test recorded
    // (SmokeReady, CompleteSmoke).
    private readonly Dictionary<string, object?> smokeScience = new();
    private System.Diagnostics.Stopwatch? smokeRestart;
    private readonly CancellationTokenSource lifetime = new();
    private readonly Dictionary<string, (string[] Paths, DateTime Expires)> fileSelections = new();
    // Claude Science host rendering: the switches this WebView was created
    // with (null: none, as always) and the frame support that needs them.
    private string? webArguments;
    private ScienceFrameHost? science;
    // Navigations ScienceFrameHost held: cancelling one still raises
    // NavigationCompleted (WebErrorStatus OperationCanceled), which is not a
    // failure to load the workbench.
    private readonly HashSet<ulong> heldNavigations = [];

    public MainWindow()
    {
        nativeInput = new NativeInput(value => Script("window.dispatchEvent(new CustomEvent('cage-native-input',{detail:" + JsonSerializer.Serialize(value) + "}));"));
        Deactivate += (_, _) => Script("window.dispatchEvent(new Event('cage-window-deactivated'));");
        AutoScaleMode = AutoScaleMode.Dpi; AutoScaleDimensions = new SizeF(96, 96);
        Text = "Super Cage";
        StartPosition = FormStartPosition.CenterScreen; BackColor = Color.FromArgb(247, 247, 247);
        Icon = Icon.ExtractAssociatedIcon(Environment.ProcessPath!) ?? SystemIcons.Application;
        status.Controls.AddRange([message, retry, existing, complete]);
        Controls.Add(web); Controls.Add(status);
        Controls.Add(nativeInput);
        retry.Click += async (_, _) => {
            if (webFailed || web.CoreWebView2 == null) await Initialize(true); else await Bootstrap();
        };
        existing.Click += async (_, _) => await Bootstrap("--existing");
        complete.Click += async (_, _) => {
            if (ConfirmStop()) await Bootstrap("--complete-update");
        };
        var trayMenu = new TrayMenu();
        trayMenu.Items.Add("打开 Super Cage", null, (_, _) => RestoreWindow());
        trayMenu.Items.Add("检查应用更新…", null, (_, _) => updater!.Handle("check"));
        trayMenu.Items.Add("关于 Super Cage", null, (_, _) => MessageBox.Show(this, $"Super Cage {AppUpdater.Version}\n本机隔离编码工作区", "关于 Super Cage"));
        trayMenu.Items.Add(new ToolStripSeparator());
        trayMenu.Items.Add("退出 Super Cage…", null, (_, _) => RequestQuit());
        trayMenu.Opening += (_, _) => (chrome ?? new WindowChrome(BackColor, ForeColor, false)).ApplyMenu(trayMenu);
        tray = new NotifyIcon { Icon = Icon, Text = "Super Cage", Visible = true, ContextMenuStrip = trayMenu };
        tray.DoubleClick += (_, _) => RestoreWindow();
        updater = new AppUpdater(PublishUpdate, () => { allowQuit = true; Close(); }, () => _ = Bootstrap());
        Shown += async (_, _) => {
            if (Program.SmokeOutput != null) _ = SmokeDeadline();
            await Initialize();
        };
        Resize += (_, _) => PublishVisibility(); VisibleChanged += (_, _) => PublishVisibility();
        FormClosing += (_, e) => {
            SaveWindowPlacement();
            if (!allowQuit && e.CloseReason is not (CloseReason.WindowsShutDown or CloseReason.TaskManagerClosing)) { e.Cancel = true; Hide(); }
        };
        FormClosed += (_, _) => { science?.EndForExit(); lifetime.Cancel(); updater.Dispose(); tray.Dispose(); trayMenu.Dispose(); web.Dispose(); };
    }

    protected override void OnLoad(EventArgs e)
    {
        base.OnLoad(e);
        WindowPlacement? saved = null;
        try { saved = JsonSerializer.Deserialize<WindowPlacement>(File.ReadAllText(Path.Combine(Backend.Root, "window-placement.json"))); }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or JsonException) { }
        var work = Screen.FromHandle(Handle).WorkingArea;
        MinimumSize = WindowLayout.Minimum(work, DeviceDpi);
        StartPosition = FormStartPosition.Manual;
        Bounds = WindowLayout.Bounds(work, DeviceDpi, saved);
        if (saved?.Maximized == true) WindowState = FormWindowState.Maximized;
    }

    private void SaveWindowPlacement()
    {
        var bounds = WindowState == FormWindowState.Normal ? Bounds : RestoreBounds;
        if (bounds.Width <= 0 || bounds.Height <= 0) return;
        try
        {
            Directory.CreateDirectory(Backend.Root);
            File.WriteAllText(Path.Combine(Backend.Root, "window-placement.json"),
                JsonSerializer.Serialize(new WindowPlacement(bounds.X, bounds.Y, bounds.Width, bounds.Height, DeviceDpi,
                    WindowState == FormWindowState.Maximized)));
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { }
    }

    protected override bool ProcessCmdKey(ref Message msg, Keys keyData)
    {
        var action = keyData switch {
            Keys.Control | Keys.O => "openProject",
            Keys.Control | Keys.T => "newTab",
            Keys.Control | Keys.Shift | Keys.Enter => "newTerminal",
            Keys.Control | Keys.W => "closeTab",
            Keys.Control | Keys.Oemcomma => "settings",
            _ => null,
        };
        if (action == null) return base.ProcessCmdKey(ref msg, keyData);
        Action(action); return true;
    }

    private WindowChrome? chrome;

    private void ApplyWindowChrome()
    {
        if (IsHandleCreated)
            chrome?.Apply(Handle, MainMenuStrip);
        if (tray?.ContextMenuStrip != null)
            chrome?.ApplyMenu(tray.ContextMenuStrip);
    }

    protected override void OnHandleCreated(EventArgs e)
    {
        base.OnHandleCreated(e);
        ApplyWindowChrome();
    }

    protected override void OnSystemColorsChanged(EventArgs e)
    {
        base.OnSystemColorsChanged(e);
        ApplyWindowChrome();
    }

    private bool ConfirmStop() => MessageBox.Show(this,
        "请先保存编辑器中的文件并结束正在运行的工作。继续后 Super Cage 会停止环境、安装已验证的更新并重新启动。",
        "保存并停止环境后安装重启", MessageBoxButtons.OKCancel, MessageBoxIcon.Information) == DialogResult.OK;

    public void RestoreWindow() { Show(); if (WindowState == FormWindowState.Minimized) WindowState = FormWindowState.Normal; Activate(); PublishVisibility(); }
    private void RequestQuit()
    {
        RestoreWindow();
        if (web.Visible) Action("quit");
        else { allowQuit = true; Close(); }
    }
    private void Action(string name) => Script("window.cageNativeAction?.(" + JsonSerializer.Serialize(name) + ")");
    private void PublishVisibility() => Script($"window.cageWindowVisible = {(Visible && WindowState != FormWindowState.Minimized ? "true" : "false")}; window.dispatchEvent(new Event('cage-window-visibility'));");
    private void PublishUpdate(Dictionary<string, object?> state) {
        if (state.TryGetValue("phase", out var phase) && phase as string == "failed") _ = Backend.Report("App update failed: " + state.GetValueOrDefault("message"));
        Script("window.dispatchEvent(new CustomEvent('cage-app-update',{detail:" + JsonSerializer.Serialize(state) + "}));");
    }
    private async void Script(string script)
    {
        if (web.IsDisposed || web.CoreWebView2 == null || IsDisposed) return;
        try { await web.ExecuteScriptAsync(script); }
        catch (InvalidOperationException) { }
        catch (System.Runtime.InteropServices.COMException) { }
    }
    private void Error(string text)
    {
        _ = Backend.Report(text);
        message.Text = text; status.Show(); status.BringToFront(); retry.Show(); web.Hide();
    }

    private async Task Initialize(bool recreate = false)
    {
        smokePhase = "initializing_webview";
        retry.Enabled = false;
        try
        {
            var arguments = ScienceFrame.Arguments(Backend.Root);
            if (recreate)
            {
                // New switches need the old browser process gone: one user
                // data folder runs with one set of options.
                var previous = arguments != webArguments ? web.CoreWebView2?.Environment : null;
                var exited = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                void BrowserExited(object? sender, CoreWebView2BrowserProcessExitedEventArgs args) => exited.TrySetResult();
                if (previous != null) previous.BrowserProcessExited += BrowserExited;
                science?.EndForExit();
                science = null;
                Controls.Remove(web); web.Dispose();
                web = new WebView2 { Dock = DockStyle.Fill, Visible = false };
                Controls.Add(web); status.BringToFront();
                if (previous != null)
                {
                    try { await exited.Task.WaitAsync(TimeSpan.FromSeconds(15), lifetime.Token); }
                    catch (TimeoutException) { }
                    finally { previous.BrowserProcessExited -= BrowserExited; }
                }
            }
            // Before the browser process starts: no network prediction with
            // the switches, the default without them.
            var data = Path.Combine(Backend.Root, "webview2");
            ScienceFrame.SeedPreferences(data, arguments != null);
            // No crash report goes to Microsoft, with or without host
            // rendering: a dump holds the workbench's tokens and the pages'
            // memory. What crash reporting wrote is deleted before each start
            // (nothing holds it then), also where a runtime ignores the option.
            CrashReports.Sweep(data);
            var options = new CoreWebView2EnvironmentOptions(arguments) { IsCustomCrashReportingEnabled = true };
            var environment = await CoreWebView2Environment.CreateAsync(null, data, options);
            // A runtime without custom crash reporting has no failure report
            // folder either (they came together): said in the diagnostics,
            // the sweep still runs.
            try { _ = environment.FailureReportFolderPath; }
            catch (Exception error) when (error is NotImplementedException or InvalidCastException or System.Runtime.InteropServices.COMException)
            {
                _ = Backend.Report("WebView2 runtime " + environment.BrowserVersionString + " has no custom crash reporting; crash dumps are deleted at start.");
            }
            webArguments = arguments;
            await web.EnsureCoreWebView2Async(environment);
            smokePhase = "webview_initialized";
            var core = web.CoreWebView2;
            core.Settings.AreDevToolsEnabled = false; core.Settings.AreDefaultContextMenusEnabled = false;
            core.Settings.IsStatusBarEnabled = false; core.Settings.IsZoomControlEnabled = false;
            core.Settings.AreBrowserAcceleratorKeysEnabled = false;
            core.Settings.IsPasswordAutosaveEnabled = false; core.Settings.IsGeneralAutofillEnabled = false;
            core.NavigationStarting += (_, e) => {
                if (!UpdatePolicy.IsWorkbench(e.Uri, expectedPort)) e.Cancel = true;
                // The pane's document is going away: its frame goes first.
                else if (science?.Hold(e.Uri, e.NavigationKind == CoreWebView2NavigationKind.Reload) == true) { e.Cancel = true; heldNavigations.Add(e.NavigationId); }
            };
            core.NewWindowRequested += (_, e) => e.Handled = true;
            core.DownloadStarting += (_, e) => e.Cancel = true;
            core.PermissionRequested += (_, e) => e.State = CoreWebView2PermissionState.Deny;
            core.WebMessageReceived += Receive;
            core.ProcessFailed += (_, e) => {
                // A frame's own process (the Claude Science frame's) leaves
                // the window's document running (N4).
                if (e.ProcessFailedKind == CoreWebView2ProcessFailedKind.FrameRenderProcessExited && science != null)
                {
                    science.FrameExited(e.FrameInfosForFailedProcess);
                    return;
                }
                science?.EndForExit();
                webFailed = true; science = null; Error("工作台显示进程已退出。后台任务保留，可重试连接。");
            };
            // What a killed or crashed window's Claude Science frame stored
            // (N6); without a journal, one existence check.
            await ScienceFrameHost.Sweep(core, Path.Combine(Backend.Root, "webview2"));
            science = arguments == null ? null : new ScienceFrameHost(core, Backend.Root, Path.Combine(Backend.Root, "webview2"), reason =>
                Script("window.dispatchEvent(new CustomEvent('cage-science-frame',{detail:{error:" + JsonSerializer.Serialize(reason) + "}}));"),
                (uri, reload) => {
                    if (web.IsDisposed || web.CoreWebView2 != core) return;
                    if (reload) core.Reload(); else core.Navigate(uri);
                });
            core.NavigationCompleted += (_, e) => {
                if (heldNavigations.Remove(e.NavigationId)) return;
                if (!e.IsSuccess) {
                    var reason = e.WebErrorStatus.ToString() switch {
                        "Timeout" => "等待本机后台响应超时",
                        "CannotConnect" => "本机后台没有接受连接，可能尚未启动或已经退出",
                        "ConnectionAborted" or "ConnectionReset" => "本机工作台连接在加载完成前中断",
                        _ => "内置网页组件未能加载本机工作台（错误类型 " + e.WebErrorStatus + "）",
                    };
                    Error("打开工作台失败：" + reason + "。请点击“重试连接”；仍失败时请保留此错误提示并反馈给维护者。后台任务不会因此自动结束。"); return;
                }
                status.Hide(); web.Show(); web.Focus(); PublishVisibility();
            };
            await core.AddScriptToExecuteOnDocumentCreatedAsync(BridgeScript);
            if (Program.SmokeOutput != null) await core.AddScriptToExecuteOnDocumentCreatedAsync(SmokeDiagnosticsScript);
            webFailed = false;
            await Bootstrap();
        }
        catch (WebView2RuntimeNotFoundException)
        {
            Error("缺少 Microsoft Edge WebView2 Runtime。请运行 Super Cage 安装程序安装随附运行时后重试。");
            webFailed = true;
        }
        catch (Exception error) { webFailed = true; Error("无法初始化工作台：" + error.Message); }
        finally { retry.Enabled = true; }
    }

    private async Task SmokeDeadline()
    {
        try { await Task.Delay(TimeSpan.FromSeconds(120), lifetime.Token); await CompleteSmoke(false, "工作台未在自检期限内确认就绪"); }
        catch (OperationCanceledException) { }
    }

    private async Task CompleteSmoke(bool nativeBridgeReady = true, string? failure = null)
    {
        if (Program.SmokeOutput == null || completingSmoke) return;
        completingSmoke = true;
        JsonElement? page = null;
        var pageURL = web.Source?.GetLeftPart(UriPartial.Path);
        var environment = web.CoreWebView2?.Environment;
        var webviewVersion = environment?.BrowserVersionString;
        using var shutdown = CancellationTokenSource.CreateLinkedTokenSource(lifetime.Token);
        shutdown.CancelAfter(TimeSpan.FromSeconds(20));
        var errors = new List<string>();
        int? prepareExit = null;
        string? prepareStatus = null;
        string? prepareMessage = null;
        if (failure != null) errors.Add(failure);
        try
        {
            if (!nativeBridgeReady && web.CoreWebView2 != null)
            {
                try
                {
                    var snapshot = await web.CoreWebView2.ExecuteScriptAsync("({title:document.title,ready:document.readyState,text:(document.body?.innerText||'').slice(0,4096),errors:window.__cageSmokeErrors||[]})");
                    page = JsonDocument.Parse(snapshot).RootElement.Clone();
                    await using var capture = File.Create(Path.ChangeExtension(Program.SmokeOutput, ".png"));
                    await web.CoreWebView2.CapturePreviewAsync(CoreWebView2CapturePreviewImageFormat.Png, capture);
                }
                catch (Exception error) { errors.Add("无法保存工作台画面：" + error.Message); }
            }
            if (nativeBridgeReady && web.CoreWebView2 != null)
            {
                smokeScience["switches"] = webArguments != null;
                if (smokeRestart != null) smokeScience["restartMilliseconds"] = smokeRestart.ElapsedMilliseconds;
                try { smokeScience["page"] = await SmokeEvaluate(SmokeScienceProbe.Replace("ASK", SmokeScience.Length != 0 ? "true" : "false")); }
                catch (Exception error) { smokeScience["pageError"] = error.Message; }
            }
            var controllers = Backend.CaptureSmokeControllers();
            try
            {
                if (nativeBridgeReady) smokePhase = "stopping_backend";
                var (exit, data) = await Backend.Json(["prepare-update", "--json"], shutdown.Token, TimeSpan.FromSeconds(20));
                prepareExit = exit;
                prepareStatus = data.Text("status") is "prepared" or "stop_required" or "error" ? data.Text("status") : "unexpected";
                // This report is uploaded by CI. Keep the shutdown error but
                // remove URL queries/fragments and bound its size.
                prepareMessage = System.Text.RegularExpressions.Regex.Replace(data.Text("message"), @"[?#][^\s""'<>)]*", "[redacted]");
                if (prepareMessage.Length > 768) prepareMessage = prepareMessage[..768];
                if (exit != 0 || data.Text("status") != "prepared") throw new IOException("自检结束时无法确认后台已停止");
                await Task.WhenAll(controllers.Select(process => process.WaitForExitAsync(shutdown.Token)));
            }
            finally { foreach (var process in controllers) process.Dispose(); }
        }
        catch (Exception error) { errors.Add(error.Message); }
        // WebView2 releases its processes asynchronously after Dispose. Its
        // browser can retain the app's working directory until this event.
        if (environment != null)
        {
            var exited = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            void BrowserExited(object? sender, CoreWebView2BrowserProcessExitedEventArgs args) => exited.TrySetResult();
            environment.BrowserProcessExited += BrowserExited;
            try
            {
                if (nativeBridgeReady && errors.Count == 0) smokePhase = "stopping_browser";
                web.Dispose();
                await exited.Task.WaitAsync(shutdown.Token);
            }
            catch (Exception error) { errors.Add("自检浏览器尚未退出：" + error.Message); }
            finally { environment.BrowserProcessExited -= BrowserExited; }
        }
        try
        {
            if (nativeBridgeReady && errors.Count == 0) smokePhase = "completed";
            File.WriteAllText(Program.SmokeOutput, JsonSerializer.Serialize(new {
                success = nativeBridgeReady && errors.Count == 0, nativeBridgeReady, version = AppUpdater.Version,
                phase = smokePhase, nativeStatus = message.Text, prepareExit, prepareStatus, prepareMessage, errors, page,
                pageURL, webviewVersion, science = smokeScience,
                windowBounds = Bounds, workingArea = Screen.FromHandle(Handle).WorkingArea, deviceDpi = DeviceDpi,
                nativeMenuBar = MainMenuStrip != null,
            }));
        }
        catch (Exception error) { Error(error.Message); }
        finally { allowQuit = true; BeginInvoke(Close); }
    }

    // Smoke only. CAGE_SMOKE_SCIENCE=on does from the page what the Claude
    // Science pane's switch and then its reload prompt do (neither needs a
    // guest) and completes in the WebView the reload made; =off turns the
    // switch off; =probe only asks. Each then asks the frame ops what the
    // pane would (SmokeScienceProbe).
    private static string SmokeScience => Environment.GetEnvironmentVariable("CAGE_SMOKE_SCIENCE") switch { "on" => "on", "off" => "off", "probe" => "probe", _ => "" };

    private async Task SmokeReady()
    {
        if (Program.SmokeOutput == null || completingSmoke) return;
        if (SmokeScience is "on" or "off" && !smokeScience.ContainsKey("switch"))
        {
            try
            {
                smokeScience["switch"] = await SmokeEvaluate(SmokeScienceSwitch.Replace("ENABLED", SmokeScience == "on" ? "true" : "false"));
                if (SmokeScience == "on" && webArguments == null && ScienceFrame.Arguments(Backend.Root) != null)
                {
                    smokeRestart = System.Diagnostics.Stopwatch.StartNew();
                    smokeScience["restart"] = "requested";
                    // Neither the script nor the bridge's replies are
                    // awaited: they race the end of this WebView. The smoke
                    // completes when the reloaded one is ready. refreshSession
                    // (a backend round trip) is still pending when the
                    // restart replaces the WebView, as any request of the
                    // page can be.
                    _ = web.CoreWebView2.ExecuteScriptAsync("const cage = window.webkit.messageHandlers.cage; void cage.postMessage({action:'refreshSession'}).catch(() => {}); void cage.postMessage({action:'scienceFrame',op:'restart'}).catch(() => {})")
                        .ContinueWith(task => _ = task.Exception, TaskScheduler.Default);
                    return;
                }
            }
            catch (Exception error) { smokeScience["switchError"] = error.Message; }
        }
        await CompleteSmoke();
    }

    // Smoke only: what a page expression evaluates to (a promise awaited).
    private async Task<JsonElement?> SmokeEvaluate(string expression)
    {
        var reply = await web.CoreWebView2.CallDevToolsProtocolMethodAsync("Runtime.evaluate",
            JsonSerializer.Serialize(new { expression, awaitPromise = true, returnByValue = true })).WaitAsync(TimeSpan.FromSeconds(30));
        using var document = JsonDocument.Parse(reply);
        if (document.RootElement.TryGetProperty("exceptionDetails", out var failure))
            throw new InvalidDataException(failure.TryGetProperty("exception", out var thrown) ? thrown.Text("description") : failure.Text("text"));
        return document.RootElement.GetProperty("result").TryGetProperty("value", out var value) ? value.Clone() : null;
    }

    private async Task Bootstrap(params string[] options)
    {
        if (bootstrapping || web.CoreWebView2 == null) return;
        bootstrapping = true;
        message.Text = "正在连接 Super Cage 工作台…"; status.Show(); status.BringToFront();
        retry.Hide(); existing.Hide(); complete.Hide();
        try
        {
            smokePhase = "starting_backend";
            var (exit, data) = await Backend.Json(["desktop", "--json", ..options], lifetime.Token);
            smokePhase = "backend_returned";
            if (exit != 0) throw new IOException(data.Text("message") is { Length: > 0 } detail ? detail : "工作台未能启动");
            controllerNeedsUpdate = data.Text("status") != "ready";
            if (data.Text("status") == "upgrade_pending")
            {
                message.Text = data.Text("message"); existing.Show(); complete.Show(); return;
            }
            var entry = UpdatePolicy.Entry(data.Text("url")); expectedPort = entry.Port;
            smokePhase = "loading_workbench";
            Text = controllerNeedsUpdate ? "Super Cage（更新待完成）" : "Super Cage";
            web.CoreWebView2.Navigate(entry.AbsoluteUri);
        }
        catch (OperationCanceledException) { if (!lifetime.IsCancellationRequested) Error("启动工作台失败：等待本机后台响应超时。请点击“重试连接”；仍失败时请保留此提示并反馈给维护者。"); }
        catch (Exception error) {
            _ = Backend.Report("Workbench startup failed: " + error);
            Error(error.Message.Any(c => c is >= '\u4E00' and <= '\u9FFF') ? error.Message : "启动工作台失败：本机后台未返回可识别的原因（错误类型 " + error.GetType().Name + "）。请点击“重试连接”；仍失败时请保留此提示并反馈给维护者。");
        }
        finally { bootstrapping = false; }
    }

    private object RememberFiles(string[] paths)
    {
        if (paths.Length is 0 or > 100) throw new InvalidDataException("请选择 1 至 100 个文件");
        foreach (var path in paths)
        {
            if (!Path.IsPathFullyQualified(path) || path.StartsWith(@"\\", StringComparison.Ordinal)) throw new InvalidDataException("请选择本机文件");
            var file = new FileInfo(path);
            if (!file.Exists || (file.Attributes & (FileAttributes.Directory | FileAttributes.ReparsePoint)) != 0)
                throw new InvalidDataException("请选择普通文件，暂不支持文件夹或链接");
            if (file.Length > 2L * 1024 * 1024 * 1024) throw new InvalidDataException("每个文件不能超过 2 GB");
        }
        var now = DateTime.UtcNow;
        foreach (var key in fileSelections.Where(item => item.Value.Expires <= now).Select(item => item.Key).ToArray()) fileSelections.Remove(key);
        if (fileSelections.Count >= 16) fileSelections.Remove(fileSelections.MinBy(item => item.Value.Expires).Key);
        var selection = Guid.NewGuid().ToString();
        fileSelections[selection] = (paths, now.AddMinutes(10));
        return new { fileSelection = selection, count = paths.Length };
    }

    // The Claude Science pane's frame (ui/src/claude-science.jsx documents
    // the ops). Attaching needs the switches; without them the pane asks for
    // restart, which recreates the WebView with them. The agent transport
    // cannot reach these ops (agentNativeAccess in ui/src/agent-native.js).
    private async Task<object> ScienceFrameRequest(string op, string url)
    {
        switch (op)
        {
            case "prepare":
                if (science == null) return new { backstop = false, proxy = "" };
                return await science.Prepare();
            case "attach":
                if (science == null) throw new InvalidDataException("窗口缺少本机渲染需要的网络限制，请重新载入窗口后再打开。");
                await science.Attach(url);
                return true;
            case "detach":
                if (science != null) await science.Detach();
                return true;
            case "restart":
                if (ScienceFrame.Arguments(Backend.Root) == webArguments) return true;
                BeginInvoke(async () => await Initialize(true));
                return true;
            default:
                throw new InvalidDataException("未知的本机渲染操作");
        }
    }

    private async void Receive(object? sender, CoreWebView2WebMessageReceivedEventArgs e)
    {
        // CoreWebView2.WebMessageReceived is top-document only. Frame messages
        // have a separate CoreWebView2Frame event, which this host never handles.
        if (!UpdatePolicy.IsWorkbench(e.Source, expectedPort)) return;
        // The reply goes to the WebView that asked. A restart (Initialize)
        // can replace it while the request is pending; the new one's
        // document never sent it, and its CoreWebView2 may not exist yet.
        var view = web;
        string id = "";
        try
        {
            var message = e.WebMessageAsJson;
            using var envelope = JsonDocument.Parse(message);
            var body = envelope.RootElement;
            id = body.Text("id");
            if (!Guid.TryParse(id, out _)) return;
            // Match the Linux bridge envelope limit. A 20 MiB PNG needs about
            // 27 MiB after Base64 encoding; retain the ID to reply on rejection.
            if (message.Length > 30 * 1024 * 1024) throw new InvalidDataException("请求内容过大，请减少图片或文本内容后重试");
            var request = body.GetProperty("request");
            object? result;
            switch (request.Text("action"))
            {
                case "remoteInput":
                    var accepted = nativeInput.ProcessRequest(request, web, web.ZoomFactor, out var returnedFocus);
                    result = request.TryGetProperty("returnFocus", out var restore) && restore.ValueKind == JsonValueKind.True
                        ? new { returnedFocus = accepted && returnedFocus } : accepted;
                    break;
                case "exportDiagnostics":
                    using (var picker = new SaveFileDialog { Title = "导出诊断包", Filter = "诊断包 (*.zip)|*.zip", FileName = "cage-diagnostics.zip", DefaultExt = "zip", AddExtension = true })
                    {
                        if (picker.ShowDialog(this) != DialogResult.OK) { result = new { canceled = true }; break; }
                        var connection = request.Text("connection_id");
                        var exported = await Backend.Json(["diagnostics", "--json", "--connection", string.IsNullOrEmpty(connection) ? "local" : connection, picker.FileName], lifetime.Token, TimeSpan.FromSeconds(60));
                        if (exported.Exit != 0) throw new IOException(exported.Data.Text("message"));
                        result = exported.Data;
                    }
                    break;
                case "workbenchReady":
                    if (controllerNeedsUpdate) throw new IOException("后台版本尚未同步，更新未完成");
                    updater.MarkReady(); result = true; _ = SmokeReady(); break;
                case "appUpdate": result = updater.Handle(request.Text("operation")); break;
                case "importTheme":
                    using (var picker = new OpenFileDialog { Filter = "Super Cage 主题 (*.json)|*.json", Title = "导入主题", Multiselect = false })
                    {
                        if (picker.ShowDialog(this) != DialogResult.OK) { result = ""; break; }
                        using var stream = File.OpenRead(picker.FileName);
                        if (stream.Length > 256 * 1024) throw new InvalidDataException("主题文件不能超过 256 KB");
                        using var reader = new StreamReader(stream, new System.Text.UTF8Encoding(false, true));
                        result = reader.ReadToEnd();
                    }
                    break;
                case "saveRecoveryKey":
                {
                    var key = request.Text("key");
                    if (key.Length != 71 || !System.Text.RegularExpressions.Regex.IsMatch(key, "^[A-F0-9]{8}(-[A-F0-9]{8}){7}$")) throw new InvalidDataException("恢复密钥格式无效");
                    using var picker = new SaveFileDialog { Filter = "文本文件 (*.txt)|*.txt", Title = "保存账户恢复密钥", FileName = "账户恢复密钥.txt", DefaultExt = "txt", AddExtension = true };
                    if (picker.ShowDialog(this) != DialogResult.OK) { result = false; break; }
                    try { File.WriteAllText(picker.FileName, key + "\n", new System.Text.UTF8Encoding(false)); result = true; }
                    catch { throw new IOException("恢复密钥未能保存，请检查保存位置的权限"); }
                    break;
                }
                case "exportTheme":
                    var themeText = request.Text("text");
                    if (System.Text.Encoding.UTF8.GetByteCount(themeText) > 256 * 1024) throw new InvalidDataException("主题文件不能超过 256 KB");
                    var themeDocument = JsonDocument.Parse(themeText).RootElement;
                    if (themeDocument.Text("format") != "cage-theme" || themeDocument.GetProperty("version").GetInt32() != 1) throw new InvalidDataException("主题文件格式无效");
                    using (var picker = new SaveFileDialog { Filter = "Super Cage 主题 (*.json)|*.json", Title = "导出主题", FileName = Path.GetFileName(request.Text("filename")), DefaultExt = "json", AddExtension = true })
                    {
                        if (picker.ShowDialog(this) != DialogResult.OK) { result = false; break; }
                        File.WriteAllText(picker.FileName, themeText, new System.Text.UTF8Encoding(false)); result = true;
                    }
                    break;
                case "shareLocalProject":
                {
                    var connection = request.Text("connection_id");
                    var accessMode = request.Text("mode");
                    if (connection.Length == 0 || accessMode is not ("read" or "write")) throw new InvalidDataException("请在云端工作台重新选择项目");
                    using var picker = new FolderBrowserDialog {
                        Description = accessMode == "write" ? "云端 Super Cage 可以读取、修改和删除所选项目中的文件。" : "云端 Super Cage 可以读取所选项目中的文件。",
                        UseDescriptionForTitle = true, ShowNewFolderButton = false
                    };
                    if (picker.ShowDialog(this) != DialogResult.OK) { result = new { canceled = true }; break; }
                    var response = await Backend.Json(["share-project", "--json", "--connection", connection, accessMode, picker.SelectedPath], lifetime.Token, TimeSpan.FromSeconds(90));
                    if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                    result = response.Data; break;
                }
                case "chooseFolder":
                    using (var picker = new FolderBrowserDialog { Description = request.Text("purpose") == "environmentStorage" ? "请选择用于保存镜像和虚拟磁盘的空文件夹。" : "仅此文件夹会与 Super Cage 共享。请选择具体项目。", UseDescriptionForTitle = true, ShowNewFolderButton = true })
                        result = picker.ShowDialog(this) == DialogResult.OK ? picker.SelectedPath : "";
                    break;
                case "clipboardRead": result = Clipboard.ContainsText() ? Clipboard.GetText() : ""; break;
                case "clipboardRevision": result = SharedClipboard.Revision; break;
                case "clipboardSyncCancel": SharedClipboard.Cancel(); result = true; break;
                case "clipboardSyncRead": result = await SharedClipboard.Read(request, lifetime.Token); break;
                case "clipboardSyncWrite": result = await SharedClipboard.Write(request, lifetime.Token); break;
                case "clipboardSyncRelease": result = await SharedClipboard.Release(request, lifetime.Token); break;
                case "desktopFileAction":
                {
                    var fileRequestID = request.Text("id");
                    if (!Guid.TryParse(fileRequestID, out _)) throw new InvalidDataException("文件打开标识无效");
                    var connection = request.Text("connection_id");
                    var response = await Backend.Json(["desktop-file", "--json", "--connection", string.IsNullOrEmpty(connection) ? "local" : connection, fileRequestID], lifetime.Token, TimeSpan.FromMinutes(30));
                    if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                    result = response.Data; break;
                }
                case "saveBrowserDownload":
                case "browserDownloadAction":
                {
                    var downloadID = request.Text("id");
                    if (!Guid.TryParse(downloadID, out _)) throw new InvalidDataException("下载标识无效");
                    var operation = request.Text("action") == "saveBrowserDownload" ? "save" : request.Text("operation");
                    if (operation is not ("save" or "open" or "reveal")) throw new InvalidDataException("下载操作无效");
                    var connection = request.Text("connection_id");
                    var arguments = new List<string> { "browser-file", "--json", "--connection", string.IsNullOrEmpty(connection) ? "local" : connection, operation, downloadID };
                    if (operation == "save")
                    {
                        var fileName = Path.GetFileName(request.Text("name"));
                        foreach (var character in Path.GetInvalidFileNameChars()) fileName = fileName.Replace(character, '_');
                        using var picker = new SaveFileDialog { Title = "将下载另存为到本机", FileName = fileName, OverwritePrompt = true, AddExtension = false };
                        if (picker.ShowDialog(this) != DialogResult.OK) { result = new { canceled = true }; break; }
                        arguments.Add(picker.FileName);
                    }
                    var response = await Backend.Json(arguments.ToArray(), lifetime.Token, TimeSpan.FromMinutes(30));
                    if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                    result = response.Data; break;
                }
                case "clipboardReadContent":
                    if (Clipboard.ContainsFileDropList()) result = RememberFiles(Clipboard.GetFileDropList().Cast<string>().ToArray());
                    else if (SharedClipboard.RichText() is { } copiedText) result = new { text = copiedText };
                    else if (Clipboard.ContainsImage())
                    {
                        using var clipboardImage = Clipboard.GetImage() ?? throw new InvalidDataException("无法读取剪贴板图片");
                        if ((long)clipboardImage.Width * clipboardImage.Height > 40_000_000) throw new InvalidDataException("图片不能超过 4000 万像素");
                        using var imageStream = new MemoryStream();
                        clipboardImage.Save(imageStream, System.Drawing.Imaging.ImageFormat.Png);
                        if (imageStream.Length > 20 * 1024 * 1024) throw new InvalidDataException("图片不能超过 20 MB");
                        result = new { png = Convert.ToBase64String(imageStream.ToArray()) };
                    }
                    else result = new { text = Clipboard.ContainsText() ? Clipboard.GetText() : "" };
                    break;
                case "chooseGuestMedia":
                    using (var picker = new OpenFileDialog { Title = "选择 Windows 11 x64 安装镜像", Filter = "Windows ISO (*.iso)|*.iso", Multiselect = false })
                        result = picker.ShowDialog(this) == DialogResult.OK ? picker.FileName : "";
                    break;
                case "appReference":
                    var appReference = request.Text("kind") switch {
                        "source" => "https://github.com/sweetcornna/Super-Cage/commits/main",
                        "releases" => "https://github.com/sweetcornna/Super-Cage/releases",
                        "claude-science-host-render" => "https://github.com/sweetcornna/Super-Cage/blob/main/docs/guide/claude-science.md#本机渲染实验性",
                        _ => throw new InvalidDataException("未知的项目页面")
                    };
                    System.Diagnostics.Process.Start(new System.Diagnostics.ProcessStartInfo(appReference) { UseShellExecute = true });
                    result = true; break;
                case "guestInstallReference":
                    var reference = request.Text("kind") switch {
                        "windows-media" => "https://www.microsoft.com/software-download/windows11",
                        "editor-license" => "https://aka.ms/vscode-server-license",
                        _ => throw new InvalidDataException("未知的安装参考页面")
                    };
                    System.Diagnostics.Process.Start(new System.Diagnostics.ProcessStartInfo(reference) { UseShellExecute = true });
                    result = true; break;
                case "chooseUploadFiles":
                    using (var picker = new OpenFileDialog { Title = "选择本机文件", Multiselect = request.TryGetProperty("multiple", out var multiple) && multiple.ValueKind == JsonValueKind.True, CheckFileExists = true })
                        result = picker.ShowDialog(this) == DialogResult.OK ? RememberFiles(picker.FileNames) : new { canceled = true };
                    break;
                case "uploadHostFiles":
                {
                    var target = request.Text("target");
                    var uploadID = request.Text("id");
                    if (target is not ("browser" or "desktop" or "terminal") || !(target == "terminal" ? System.Text.RegularExpressions.Regex.IsMatch(uploadID, "^[a-f0-9]{16}$") : Guid.TryParse(uploadID, out _))) throw new InvalidDataException("文件上传目标无效");
                    if (!fileSelections.Remove(request.Text("selection"), out var selected) || selected.Expires <= DateTime.UtcNow)
                        throw new InvalidDataException("文件选择已过期，请重新复制或选择文件");
                    var connection = request.Text("connection_id");
                    var arguments = new List<string> { "upload-files", "--json", "--connection", string.IsNullOrEmpty(connection) ? "local" : connection, target, uploadID };
                    arguments.AddRange(selected.Paths);
                    var response = await Backend.Json(arguments.ToArray(), lifetime.Token, TimeSpan.FromMinutes(30));
                    if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                    result = response.Data;
                    break;
                }
                case "clipboardWrite":
                    if (request.Text("text") is { Length: > 0 } text) Clipboard.SetText(text); else Clipboard.Clear();
                    result = true; break;
                case "appearance":
                    var mode = request.Text("mode"); var background = request.Text("background");
                    if (mode is not ("system" or "light" or "dark") || !System.Text.RegularExpressions.Regex.IsMatch(background, "^#[0-9a-fA-F]{6}$"))
                        throw new InvalidDataException("外观参数无效");
                    var foreground = request.Text("foreground");
                    BackColor = ColorTranslator.FromHtml(background); web.DefaultBackgroundColor = BackColor;
                    var ink = System.Text.RegularExpressions.Regex.IsMatch(foreground, "^#[0-9a-fA-F]{6}$")
                        ? ColorTranslator.FromHtml(foreground) : WindowChrome.ContrastingText(BackColor);
                    chrome = new WindowChrome(BackColor, ink, request.Text("variant") == "dark");
                    ApplyWindowChrome();
                    web.CoreWebView2.Profile.PreferredColorScheme = mode == "dark" ? CoreWebView2PreferredColorScheme.Dark : mode == "light" ? CoreWebView2PreferredColorScheme.Light : CoreWebView2PreferredColorScheme.Auto;
                    result = true; break;
                case "accountDevice":
                    if (request.Text("operation") != "status") throw new InvalidDataException("此平台暂不支持快捷登录");
                    result = new { supported = false }; break;
                case "refreshSession":
                    var (exit, data) = await Backend.Json(controllerNeedsUpdate ? ["desktop", "--json", "--existing"] : ["desktop", "--json"], lifetime.Token);
                    if (exit != 0) throw new IOException("工作台会话恢复失败");
                    result = new { url = UpdatePolicy.Entry(data.Text("url")).AbsoluteUri }; break;
                case "reconnect": result = true; _ = Bootstrap(); break;
                case "scienceFrame": result = await ScienceFrameRequest(request.Text("op"), request.Text("url")); break;
                case "quitNow": result = true; allowQuit = true; BeginInvoke(Close); break;
                default: throw new InvalidDataException("未知原生操作");
            }
            Reply(view, JsonSerializer.Serialize(new { id, result }));
        }
        catch (Exception error)
        {
            _ = Backend.Report(error.ToString());
            if (id.Length != 0) Reply(view, JsonSerializer.Serialize(new { id, error = error.Message }));
        }
    }

    private void Reply(WebView2 view, string json)
    {
        if (!IsDisposed && view == web && !view.IsDisposed && view.CoreWebView2 is { } core) core.PostWebMessageAsJson(json);
    }

    // Smoke only: the pane's requests (ui/src/claude-science.jsx), as the
    // page sends them. Nothing in the reports is an address or a credential.
    private const string SmokeScienceRequests = """
          const context = await (await fetch('/api/client/context')).json();
          const headers = {'Content-Type': 'application/json', 'X-Cage-Connection': context.connection_id || 'disconnected'};
          const call = async (path, body) => {
            const response = await fetch('/api' + path, body === undefined ? {headers} : {method: 'POST', headers, body: JSON.stringify(body)});
            const text = await response.text();
            let value = null;
            try { value = JSON.parse(text); } catch {}
            return {status: response.status, value: value || {}, text: response.ok ? '' : text.slice(0, 300)};
          };
          const state = reply => ({status: reply.status, available: reply.value.available, code: reply.value.unavailable_code || '',
            enabled: reply.value.enabled, running: reply.value.running, error: reply.value.error || reply.text});
        """;

    private const string SmokeScienceSwitch = "(async () => {" + SmokeScienceRequests + """
          return state(await call('/claude-science/host-render', {enabled: ENABLED, contract: window.cageNativeScienceFrame}));
        })()
        """;

    private const string SmokeScienceProbe = """
        (async () => {
          const report = {contract: window.cageNativeScienceFrame ?? null, moveBefore: 'moveBefore' in Element.prototype,
            frames: document.querySelectorAll('iframe').length};
          if (!ASK) return report;
        """ + SmokeScienceRequests + """
          const bridge = window.webkit.messageHandlers.cage;
          const ask = async request => {
            try { return {result: await bridge.postMessage(request)}; }
            catch (error) { return {error: String(error && error.message).slice(0, 300)}; }
          };
          report.prepare = await ask({action: 'scienceFrame', op: 'prepare'});
          report.hostRender = state(await call('/claude-science/host-render'));
          const ticket = await call('/claude-science/ticket', {host: {timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || '',
            offset: new Date().getTimezoneOffset(), languages: [...(navigator.languages || [])].slice(0, 8)},
            proxy: String(report.prepare.result?.proxy || ''), contract: window.cageNativeScienceFrame});
          report.ticket = {status: ticket.status, url: Boolean(ticket.value.url), error: ticket.text};
          report.attachUnknown = await ask({action: 'scienceFrame', op: 'attach', url: 'http://' + '0'.repeat(32) + '.localhost:9/_cage/enter'});
          report.detach = await ask({action: 'scienceFrame', op: 'detach'});
          report.frames = document.querySelectorAll('iframe').length;
          return report;
        })()
        """;

    private const string SmokeDiagnosticsScript = """
        (() => {
          if (window !== window.top || location.protocol !== 'http:' || location.hostname !== 'localhost') return;
          const errors = [];
          Object.defineProperty(window, '__cageSmokeErrors', { value: errors });
          const ownPath = value => {
            try { const url = new URL(value, location.href); return url.origin === location.origin ? url.pathname : ''; }
            catch { return ''; }
          };
          const scrub = value => {
            let text = String(value || '');
            for (const input of document.querySelectorAll('input,textarea'))
              if (input.value) text = text.split(input.value).join('[form-value]');
            return text.replace(/https?:\/\/[^\s"'<>`]+/g, value => ownPath(value) || '[external]')
              .replace(/[?#][^\s"'<>)]*/g, '[redacted]').slice(0, 768);
          };
          const stack = error => String(error?.stack || '').split('\n').slice(1)
            .filter(line => line.includes(location.origin + '/')).slice(0, 6).map(scrub);
          const record = value => { if (errors.length < 8) errors.push(value); };
          addEventListener('error', event => {
            if (event.target instanceof HTMLScriptElement || event.target instanceof HTMLLinkElement) {
              const source = ownPath(event.target.src || event.target.href);
              if (source) record({ type: 'module-load', source });
              return;
            }
            const source = ownPath(event.filename || location.href);
            if (source) record({ type: 'error', source, message: scrub(event.message), stack: stack(event.error) });
          }, true);
          addEventListener('unhandledrejection', event => {
            const frames = stack(event.reason);
            // Primitive/cross-origin rejections are counted without exporting arbitrary values.
            record({ type: 'unhandledrejection', message: frames.length ? scrub(event.reason?.message) : 'Unhandled rejection', stack: frames });
          });
        })();
        """;

    // The workbench reads the Claude Science frame contract from
    // window.cageNativeScienceFrame, so it never has to ask a window that
    // has no scienceFrame op (and files no error there).
    private static readonly string BridgeScript = """
        (() => {
          if (window !== window.top || location.protocol !== 'http:' || location.hostname !== 'localhost') return;
          window.cageNativeIMEAvailable = true;
          Object.defineProperty(window, 'cageNativeScienceFrame', {value: SCIENCE_FRAME_CONTRACT, writable: false});
          const pending = new Map();
          const fileActions = new Set([
            'uploadHostFiles', 'chooseUploadFiles', 'clipboardSyncRead', 'clipboardSyncWrite',
            'saveBrowserDownload', 'browserDownloadAction', 'desktopFileAction', 'exportDiagnostics',
            'importTheme', 'exportTheme', 'saveRecoveryKey', 'shareLocalProject', 'chooseFolder', 'chooseGuestMedia',
          ]);
          chrome.webview.addEventListener('message', ({data}) => {
            const item = pending.get(data.id); if (!item) return;
            pending.delete(data.id); clearTimeout(item.timer);
            data.error ? item.reject(new Error(data.error)) : item.resolve(data.result);
          });
          const cage = {postMessage(request) { return new Promise((resolve, reject) => {
            const id = crypto.randomUUID();
            // File commands have a 30-minute backend deadline. Leave time for
            // the native file dialog and the final subprocess response.
            const timer = setTimeout(() => { pending.delete(id); reject(new Error('原生操作超时，请重试')); }, fileActions.has(request.action) ? 3600000 : 240000);
            pending.set(id, {resolve, reject, timer}); chrome.webview.postMessage({id, request});
          }); }};
          Object.defineProperty(window, 'webkit', {value: {messageHandlers: {cage}}, writable: false});
        })();
        """.Replace("SCIENCE_FRAME_CONTRACT", ScienceFrame.Contract.ToString(System.Globalization.CultureInfo.InvariantCulture));
}
